package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openai/openai-go"
)

type cliCommand struct {
	name        string
	description string
	callback    func(c *config, args ...string) error
}

type config struct {
}

func commandExit(c *config, args ...string) error {
	fmt.Print("Closing the Pokedex... Goodbye!")
	os.Exit(0)
	return nil
}
func commandHelp(c *config, args ...string) error {
	fmt.Println("Welcome to the Pokedex!")
	fmt.Println("Usage:")
	fmt.Println("help: Displays a help message")
	fmt.Println("exit: Exit the Pokedex")
	return nil
}

var commands = map[string]cliCommand{
	"exit": {
		name:        "exit",
		description: "Exit the Pokedex",
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

func main() {
	fmt.Println("Welcome to G.O.A.T agent")

	scanner := bufio.NewScanner(os.Stdin)
	client := openai.NewClient(
		option.WithAPIKey("My API Key"), // or set OPENAI_API_KEY in your env
	)

	resp, err := client.Responses.New(context.TODO(), openai.ResponseNewParams{
		Model: "gpt-5.4",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("Say this is a test")},
	})
	if err != nil {
		panic(err.Error())
	}

	fmt.Println(resp.OutputText())
	for {
		fmt.Print("G.O.A.T > ")
		scanner.Scan()
		text := scanner.Text()
		words := cleanInput(text)
		if len(words) == 0 {
			continue
		}
	}
}
