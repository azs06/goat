package main

import (
	"bufio"
	"context"
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
		Model: "gpt-5.4-mini",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String(p)},
	})
	if err != nil {
		panic(err.Error())
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
			sendPrompt(ctx, &client, text)
		}

	}
}
