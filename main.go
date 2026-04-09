package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
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

func readDir() {

}

func readFile() {

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
	return []responses.ToolUnionParam{{
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
	}}
}

func executeToolCall(toolCall responses.ResponseFunctionToolCall) (string, error) {
	if toolCall.Name != "get_weather" {
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}

	var args struct {
		Location string `json:"location"`
	}

	if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err != nil {
		return "", fmt.Errorf("decode tool arguments: %w", err)
	}

	if strings.TrimSpace(args.Location) == "" {
		return "", fmt.Errorf("missing location")
	}

	return getWeather(args.Location), nil
}

func sendPromptStream(ctx context.Context, c *openai.Client, p string) {
	resp, err := c.Responses.New(ctx, responses.ResponseNewParams{
		Model:        "gpt-5.4-mini",
		Instructions: openai.String(SystemPrompt),
		Input:        responses.ResponseNewParamsInputUnion{OfString: openai.String(p)},
		Tools:        getTools(),
	})
	if err != nil {
		panic(err.Error())
	}

	for _, item := range resp.Output {
		if item.Type != "function_call" {
			continue
		}

		toolCall := item.AsFunctionCall()
		toolOutput, err := executeToolCall(toolCall)
		if err != nil {
			panic(err.Error())
		}

		resp, err = c.Responses.New(ctx, responses.ResponseNewParams{
			Model:              "gpt-5.4-mini",
			Instructions:       openai.String(SystemPrompt),
			PreviousResponseID: openai.String(resp.ID),
			Input: responses.ResponseNewParamsInputUnion{
				OfInputItemList: []responses.ResponseInputItemUnionParam{{
					OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
						CallID: toolCall.CallID,
						Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
							OfString: openai.String(toolOutput),
						},
					},
				}},
			},
			Tools: getTools(),
		})
		if err != nil {
			panic(err.Error())
		}
	}

	fmt.Println(resp.OutputText())
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
