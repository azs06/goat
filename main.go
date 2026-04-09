package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

const bashToolTimeout = 30 * time.Second

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

func editFile(filePath, oldText, newText string, replaceAll bool) (int, error) {
	if strings.TrimSpace(filePath) == "" {
		return 0, fmt.Errorf("missing path")
	}
	if oldText == "" {
		return 0, fmt.Errorf("old_text must not be empty")
	}

	content, err := readFile(filePath)
	if err != nil {
		return 0, err
	}

	matchCount := strings.Count(content, oldText)
	if matchCount == 0 {
		return 0, fmt.Errorf("old_text not found in %s", filepath.Clean(filePath))
	}
	if matchCount > 1 && !replaceAll {
		return 0, fmt.Errorf("old_text matched %d times in %s; set replace_all to true to replace every match", matchCount, filepath.Clean(filePath))
	}

	updatedContent := content
	replaced := 1
	if replaceAll {
		updatedContent = strings.ReplaceAll(content, oldText, newText)
		replaced = matchCount
	} else {
		updatedContent = strings.Replace(content, oldText, newText, 1)
	}

	if err := writeFile(filePath, updatedContent); err != nil {
		return 0, err
	}

	return replaced, nil
}

func runBashCommand(ctx context.Context, command, workdir string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("missing command")
	}

	if strings.TrimSpace(workdir) == "" {
		workdir = "."
	}

	resolvedDir, relDir, err := resolveToolPath(workdir)
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, bashToolTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", "-lc", command)
	cmd.Dir = resolvedDir

	output, err := cmd.CombinedOutput()
	trimmedOutput := strings.TrimRight(string(output), "\n")
	if trimmedOutput == "" {
		trimmedOutput = "(no output)"
	}

	result := fmt.Sprintf("Bash output for %q in %s:\n%s", command, relDir, trimmedOutput)

	if runCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("command timed out after %s\n%s", bashToolTimeout, result)
	}
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, result)
	}

	return result, nil
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

func newResponseParams(input responses.ResponseNewParamsInputUnion, previousResponseID string) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model:        "gpt-5.4-mini",
		Instructions: openai.String(SystemPrompt),
		Input:        input,
		Tools:        getTools(),
		Reasoning: shared.ReasoningParam{
			Effort: shared.ReasoningEffortHigh,
		},
	}

	if strings.TrimSpace(previousResponseID) != "" {
		params.PreviousResponseID = openai.String(previousResponseID)
	}

	return params
}

func createStreamedResponse(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
	stream := c.Responses.NewStreaming(ctx, params)
	defer stream.Close()

	var (
		finalResponse *responses.Response
		printedText   bool
	)

	for stream.Next() {
		switch event := stream.Current().AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			fmt.Print(event.Delta)
			printedText = true
		case responses.ResponseCompletedEvent:
			response := event.Response
			finalResponse = &response
		}
	}

	if err := stream.Err(); err != nil {
		return nil, printedText, err
	}
	if finalResponse == nil {
		return nil, printedText, fmt.Errorf("stream completed without a final response")
	}

	return finalResponse, printedText, nil
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
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "edit_file",
				Description: openai.String("Edit an existing workspace file by replacing exact text"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]string{
							"type": "string",
						},
						"old_text": map[string]string{
							"type": "string",
						},
						"new_text": map[string]string{
							"type": "string",
						},
						"replace_all": map[string]string{
							"type": "boolean",
						},
					},
					"required": []string{"path", "old_text", "new_text"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "run_bash",
				Description: openai.String("Run a bash command inside the workspace and return combined stdout and stderr"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]string{
							"type": "string",
						},
						"workdir": map[string]string{
							"type": "string",
						},
					},
					"required": []string{"command"},
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

func executeToolCall(ctx context.Context, toolCall responses.ResponseFunctionToolCall) (string, error) {
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
	case "edit_file":
		var args struct {
			Path       string `json:"path"`
			OldText    string `json:"old_text"`
			NewText    string `json:"new_text"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		replaced, err := editFile(args.Path, args.OldText, args.NewText, args.ReplaceAll)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s edited successfully. Replaced %d occurrence(s).", filepath.Clean(args.Path), replaced), nil
	case "run_bash":
		var args struct {
			Command string `json:"command"`
			Workdir string `json:"workdir"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		return runBashCommand(ctx, args.Command, args.Workdir)
	default:
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}
}

func sendPromptStream(ctx context.Context, c *openai.Client, p string) {
	resp, printedText, err := createStreamedResponse(ctx, c, newResponseParams(
		responses.ResponseNewParamsInputUnion{OfString: openai.String(p)},
		"",
	))
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
			toolOutput, err := executeToolCall(ctx, toolCall)
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

		if printedText {
			fmt.Println()
		}

		if len(toolOutputs) == 0 {
			return
		}

		resp, printedText, err = createStreamedResponse(ctx, c, newResponseParams(
			responses.ResponseNewParamsInputUnion{
				OfInputItemList: toolOutputs,
			},
			resp.ID,
		))
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
