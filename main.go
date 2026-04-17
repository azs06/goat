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

const defaultModel = "gpt-5.4"

type cliCommand struct {
	name        string
	description string
	callback    func(c *config, args ...string) error
}

type config struct {
	model      string
	responseID string
}

func loadConfig() *config {
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = defaultModel
	}

	return &config{model: model}
}

func commandExit(c *config, args ...string) error {
	fmt.Print("Closing the Goat... Goodbye!")
	os.Exit(0)
	return nil
}

func commandHelp(c *config, args ...string) error {
	fmt.Println("Welcome to the Goat!")
	fmt.Println("Available commands:")
	fmt.Println("- exit: Exit the Goat")
	fmt.Println("- help: Display available commands")
	fmt.Println("- history: Show the conversation history path")
	fmt.Println("- model: Show the active model")
	fmt.Println("- reset: Clear the conversation context")
	fmt.Println("- tools: List available tools")
	return nil
}

func commandReset(c *config, args ...string) error {
	c.responseID = ""
	fmt.Println("Conversation context cleared.")
	return nil
}

func commandModel(c *config, args ...string) error {
	fmt.Printf("Active model: %s\n", c.model)
	return nil
}

func commandTools(c *config, args ...string) error {
	fmt.Println("Available tools:")
	for _, name := range availableToolNames() {
		fmt.Printf("- %s\n", name)
	}
	return nil
}

func commandHistory(c *config, args ...string) error {
	resolvedPath, relPath, err := resolveToolPath(conversationHistoryPath())
	if err != nil {
		return err
	}

	if info, err := os.Stat(resolvedPath); err == nil {
		fmt.Printf("Conversation history: %s (%d bytes)\n", relPath, info.Size())
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	fmt.Printf("Conversation history: %s (not created yet)\n", relPath)
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
	"history": {
		name:        "history",
		description: "Show the conversation history path",
		callback:    commandHistory,
	},
	"model": {
		name:        "model",
		description: "Show the active model",
		callback:    commandModel,
	},
	"reset": {
		name:        "reset",
		description: "Clear the conversation context",
		callback:    commandReset,
	},
	"tools": {
		name:        "tools",
		description: "List available tools",
		callback:    commandTools,
	},
}

func cleanInput(text string) []string {
	text = strings.TrimSpace(text)
	text = strings.ToLower(text)
	words := strings.Fields(text)
	return words
}

func sendPrompt(ctx context.Context, c *openai.Client, model, prompt string) {
	resp, err := c.Responses.New(ctx, responses.ResponseNewParams{
		Model:        model,
		Instructions: openai.String(SystemPrompt),
		Input:        responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)},
	})
	if err != nil {
		panic(err.Error())
	}

	fmt.Println(resp.OutputText())
}

func newResponseParams(model string, input responses.ResponseNewParamsInputUnion, previousResponseID string) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model:        model,
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

var streamedResponseCreator = createStreamedResponse

func availableToolNames() []string {
	return []string{
		"read_dir",
		"tree_dir",
		"read_file",
		"write_file",
		"edit_file",
		"copy_file",
		"move_file",
		"delete_file",
		"run_bash",
		"find_files",
		"grep_files",
	}
}

func getTools() []responses.ToolUnionParam {
	return []responses.ToolUnionParam{
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "read_dir",
				Description: openai.String("List files and directories for a relative path in the workspace"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "tree_dir",
				Description: openai.String("Render a recursive directory tree for a relative workspace path."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"limit": map[string]any{
							"type":    "integer",
							"minimum": 1,
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
						"path": map[string]any{
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
				Description: openai.String("Write or overwrite a file at a relative path in the workspace with provided content. Set preview=true to get a diff without changing the file."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"content": map[string]any{
							"type": "string",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "edit_file",
				Description: openai.String("Edit an existing workspace file by replacing exact text. Set preview=true to get a diff without changing the file."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"old_text": map[string]any{
							"type": "string",
						},
						"new_text": map[string]any{
							"type": "string",
						},
						"replace_all": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path", "old_text", "new_text"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "copy_file",
				Description: openai.String("Copy a file to another relative workspace path. Set preview=true to inspect the destination diff first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source_path": map[string]any{
							"type": "string",
						},
						"destination_path": map[string]any{
							"type": "string",
						},
						"overwrite": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"source_path", "destination_path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "move_file",
				Description: openai.String("Move a file to another relative workspace path. Set preview=true to inspect the destination diff first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source_path": map[string]any{
							"type": "string",
						},
						"destination_path": map[string]any{
							"type": "string",
						},
						"overwrite": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"source_path", "destination_path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "delete_file",
				Description: openai.String("Delete a file from the workspace. Set preview=true to inspect a deletion diff first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path"},
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
						"command": map[string]any{
							"type": "string",
						},
						"workdir": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "find_files",
				Description: openai.String("Recursively find files within the workspace. Pattern can be a glob like *.go or a plain substring."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"pattern": map[string]any{
							"type": "string",
						},
						"limit": map[string]any{
							"type":    "integer",
							"minimum": 1,
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "grep_files",
				Description: openai.String("Recursively search for text inside files in the workspace and return matching lines."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"query": map[string]any{
							"type": "string",
						},
						"case_sensitive": map[string]any{
							"type": "boolean",
						},
						"limit": map[string]any{
							"type":    "integer",
							"minimum": 1,
						},
					},
					"required": []string{"path", "query"},
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

func prettyToolArguments(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return ""
	}

	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return raw
	}

	formatted, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return raw
	}

	return string(formatted)
}

func indentBlock(text, prefix string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func truncateForLog(text string, max int) string {
	if len(text) <= max {
		return text
	}
	if max < 4 {
		return text[:max]
	}
	return text[:max-3] + "..."
}

func logToolCallStart(toolCall responses.ResponseFunctionToolCall) {
	fmt.Printf("\n==> tool: %s\n", toolCall.Name)
	if args := prettyToolArguments(toolCall.Arguments); args != "" {
		fmt.Println(indentBlock(args, "    "))
	}
}

func logToolCallResult(name, output string, err error) {
	status := "ok"
	body := output
	if err != nil {
		status = "error"
		body = err.Error()
	}

	fmt.Printf("<== tool: %s [%s]\n", name, status)
	if strings.TrimSpace(body) != "" {
		fmt.Println(indentBlock(truncateForLog(body, 700), "    "))
	}
}

func executeToolCall(ctx context.Context, toolCall responses.ResponseFunctionToolCall) (string, error) {
	switch toolCall.Name {
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
	case "tree_dir":
		var args struct {
			Path  string `json:"path"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		lines, truncated, err := treeDir(args.Path, args.Limit)
		if err != nil {
			return "", err
		}
		message := fmt.Sprintf("Directory tree for %s:\n%s", filepath.Clean(args.Path), strings.Join(lines, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d line(s))", len(lines))
		}
		return message, nil
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
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("missing path")
		}
		if args.Preview {
			return previewWriteFile(args.Path, args.Content)
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
			Preview    bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewEditFile(args.Path, args.OldText, args.NewText, args.ReplaceAll)
		}
		replaced, err := editFile(args.Path, args.OldText, args.NewText, args.ReplaceAll)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s edited successfully. Replaced %d occurrence(s).", filepath.Clean(args.Path), replaced), nil
	case "copy_file":
		var args struct {
			SourcePath      string `json:"source_path"`
			DestinationPath string `json:"destination_path"`
			Overwrite       bool   `json:"overwrite"`
			Preview         bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewCopyFile(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := copyFile(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s copied to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "move_file":
		var args struct {
			SourcePath      string `json:"source_path"`
			DestinationPath string `json:"destination_path"`
			Overwrite       bool   `json:"overwrite"`
			Preview         bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewMoveFile(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := moveFile(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s moved to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "delete_file":
		var args struct {
			Path    string `json:"path"`
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewDeleteFile(args.Path)
		}
		if err := deleteFile(args.Path); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s deleted successfully.", filepath.Clean(args.Path)), nil
	case "run_bash":
		var args struct {
			Command string `json:"command"`
			Workdir string `json:"workdir"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		return runBashCommand(ctx, args.Command, args.Workdir)
	case "find_files":
		var args struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
			Limit   int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		matches, truncated, err := findFiles(args.Path, args.Pattern, args.Limit)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			if strings.TrimSpace(args.Pattern) == "" {
				return fmt.Sprintf("No files found under %s.", filepath.Clean(args.Path)), nil
			}
			return fmt.Sprintf("No files found for pattern %q under %s.", args.Pattern, filepath.Clean(args.Path)), nil
		}

		message := fmt.Sprintf("Found %d file(s) under %s:\n%s", len(matches), filepath.Clean(args.Path), strings.Join(matches, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d result(s))", len(matches))
		}
		return message, nil
	case "grep_files":
		var args struct {
			Path          string `json:"path"`
			Query         string `json:"query"`
			CaseSensitive bool   `json:"case_sensitive"`
			Limit         int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		matches, truncated, err := grepFiles(args.Path, args.Query, args.CaseSensitive, args.Limit)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return fmt.Sprintf("No matches found for %q under %s.", args.Query, filepath.Clean(args.Path)), nil
		}

		message := fmt.Sprintf("Found %d match(es) for %q under %s:\n%s", len(matches), args.Query, filepath.Clean(args.Path), strings.Join(matches, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d match(es))", len(matches))
		}
		return message, nil
	default:
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}
}

func sendPromptStream(ctx context.Context, c *openai.Client, prompt, responseID, model string) string {
	recordHistory(historyEntry{Type: "user", Content: prompt})

	resp, printedText, err := streamedResponseCreator(ctx, c, newResponseParams(
		model,
		responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)},
		responseID,
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
			logToolCallStart(toolCall)
			recordHistory(historyEntry{Type: "tool_call", ToolName: toolCall.Name, Arguments: toolCall.Arguments, ResponseID: resp.ID})

			toolOutput, err := executeToolCall(ctx, toolCall)
			logToolCallResult(toolCall.Name, toolOutput, err)
			if err != nil {
				recordHistory(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: err.Error(), ResponseID: resp.ID, Status: "error"})
				toolOutput = fmt.Sprintf("Tool %s failed: %v", toolCall.Name, err)
			} else {
				recordHistory(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: toolOutput, ResponseID: resp.ID, Status: "ok"})
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
			recordHistory(historyEntry{Type: "assistant", Content: resp.OutputText(), ResponseID: resp.ID})
			return resp.ID
		}

		resp, printedText, err = streamedResponseCreator(ctx, c, newResponseParams(
			model,
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

	cfg := loadConfig()
	apiKey := os.Getenv("OPENAI_API_KEY")
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
			err := cmd.callback(cfg, words[1:]...)
			if err != nil {
				fmt.Println("Error:", err)
			}
		} else {
			cfg.responseID = sendPromptStream(ctx, &client, text, cfg.responseID, cfg.model)
		}
	}
}
