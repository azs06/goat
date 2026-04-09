package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type cliCommand struct {
	name        string
	description string
	callback    func(c *config, args ...string) error
}

type config struct {
}

func commandExit(c *config, args ...string) error {
	fmt.Print("Closing the Goat... Goodbye!")
	os.Exit(0)
	return nil
}
func commandHelp(c *config, args ...string) error {
	fmt.Println("Welcome to the Goat!")
	fmt.Println("exit: Exit the Agent")
	return nil
}

var commands = map[string]cliCommand{
	"exit": {
		name:        "exit",
		description: "Exit the Goat",
		callback:    commandExit,
	},
	"help": {
		name:        "help",
		description: "Display available commands",
		callback:    commandHelp,
	},
}

func resolveToolPath(path string) (string, string, error) {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	if cleanPath == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(cleanPath) {
		return "", "", fmt.Errorf("path must be relative to the workspace")
	}

	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("determine workspace root: %w", err)
	}

	resolvedPath := filepath.Join(workspaceRoot, cleanPath)
	relPath, err := filepath.Rel(workspaceRoot, resolvedPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("path escapes the workspace")
	}

	return resolvedPath, relPath, nil
}

func readDir(dirPath string) ([]string, error) {
	resolvedPath, _, err := resolveToolPath(dirPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(resolvedPath)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	return names, nil
}

func readFile(filePath string) (string, error) {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeFile(filePath, content string) error {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	return os.WriteFile(resolvedPath, []byte(content), 0644)
}

func cleanInput(text string) []string {
	text = strings.TrimSpace(text) // remove leading/trailing whitespace
	text = strings.ToLower(text)   // normalize case
	words := strings.Fields(text)  // split by any whitespace, ignoring multiples
	return words
}

func sendPrompt(ctx context.Context, c *openai.Client, p string) {
	resp, err := c.Responses.New(ctx, responses.ResponseNewParams{
		Model:        "gpt-5.4-mini",
		Instructions: openai.String(SystemPrompt),
		Input:        responses.ResponseNewParamsInputUnion{OfString: openai.String(p)},
	})
	if err != nil {
		panic(err.Error())
	}

	fmt.Println(resp.OutputText())
}

func getWeather(location string) string {
	return fmt.Sprintf("The weather in %s is 72F and sunny.", location)
}

func getTools() []responses.ToolUnionParam {
	return []responses.ToolUnionParam{
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "get_weather",
				Description: openai.String("Get weather at the given location"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]string{
							"type": "string",
						},
					},
					"required": []string{"location"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "read_dir",
				Description: openai.String("List files and directories for a relative path in the workspace"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]string{
							"type": "string",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "read_file",
				Description: openai.String("Read a file from a relative path in the workspace"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]string{
							"type": "string",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "write_file",
				Description: openai.String("Write or overwrite a file at a relative path in the workspace with provided content"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]string{
							"type": "string",
						},
						"content": map[string]string{
							"type": "string",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
	}
}

func decodeToolArguments(raw string, target any) error {
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	return nil
}

func executeToolCall(toolCall responses.ResponseFunctionToolCall) (string, error) {
	switch toolCall.Name {
	case "get_weather":
		var args struct {
			Location string `json:"location"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Location) == "" {
			return "", fmt.Errorf("missing location")
		}
		return getWeather(args.Location), nil
	case "read_dir":
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		entries, err := readDir(args.Path)
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return fmt.Sprintf("Directory listing for %s:\n(empty)", filepath.Clean(args.Path)), nil
		}
		return fmt.Sprintf("Directory listing for %s:\n%s", filepath.Clean(args.Path), strings.Join(entries, "\n")), nil
	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		content, err := readFile(args.Path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File contents for %s:\n%s", filepath.Clean(args.Path), content), nil
	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("missing path")
		}
		if err := writeFile(args.Path, args.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s written successfully.", filepath.Clean(args.Path)), nil
	default:
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}
}

func sendPromptStream(ctx context.Context, c *openai.Client, p string) {
	resp, err := c.Responses.New(ctx, responses.ResponseNewParams{
		Model:        "gpt-5.4-mini",
		Instructions: openai.String(SystemPrompt),
		Input:        responses.ResponseNewParamsInputUnion{OfString: openai.String(p)},
		Tools:        getTools(),
		Reasoning: shared.ReasoningParam{
			Effort: shared.ReasoningEffortHigh,
		},
	})
	if err != nil {
		panic(err.Error())
	}

	for {
		var toolOutputs []responses.ResponseInputItemUnionParam

		for _, item := range resp.Output {
			if item.Type != "function_call" {
				continue
			}

			toolCall := item.AsFunctionCall()
			toolOutput, err := executeToolCall(toolCall)
			if err != nil {
				toolOutput = fmt.Sprintf("Tool %s failed: %v", toolCall.Name, err)
			}

			toolOutputs = append(toolOutputs, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: toolCall.CallID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(toolOutput),
					},
				},
			})
		}

		if len(toolOutputs) == 0 {
			fmt.Println(resp.OutputText())
			return
		}

		resp, err = c.Responses.New(ctx, responses.ResponseNewParams{
			Model:              "gpt-5.4-mini",
			Instructions:       openai.String(SystemPrompt),
			PreviousResponseID: openai.String(resp.ID),
			Input: responses.ResponseNewParamsInputUnion{
				OfInputItemList: toolOutputs,
			},
			Tools: getTools(),
			Reasoning: shared.ReasoningParam{
				Effort: shared.ReasoningEffortHigh,
			},
		})
		if err != nil {
			panic(err.Error())
		}
	}
}

func main() {
	ctx := context.Background()
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	var apiKey = os.Getenv("OPENAI_API_KEY")
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
	)
	fmt.Println("Welcome to G.O.A.T agent")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("G.O.A.T > ")
		scanner.Scan()
		text := scanner.Text()
		words := cleanInput(text)
		if len(words) == 0 {
			continue
		}

		command := words[0]

		if cmd, ok := commands[command]; ok {
			err := cmd.callback(&config{}, words[1:]...)
			if err != nil {
				fmt.Println("Error:", err)
			}
		} else {
			//sendPrompt(ctx, &client, text)
			sendPromptStream(ctx, &client, text)
		}

	}
}
