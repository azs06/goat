package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/workspace"
)

// commandProcesses lists the commands goat left running in the background, such as a
// dev server started with "&", and stops them: /ps picks one on a terminal, /ps stop N
// or /ps stop all stops them by number.
func commandProcesses(ctx context.Context, c *config, args []string) error {
	groups := workspace.BackgroundGroups()
	if len(args) > 0 {
		if args[0] != "stop" || len(args) != 2 {
			return errors.New("usage: /ps, /ps stop <n> or /ps stop all")
		}
		if len(groups) == 0 {
			return errors.New("nothing goat started is running in the background")
		}
		if args[1] == "all" {
			return stopProcessGroups(groups)
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 || n > len(groups) {
			return fmt.Errorf("choose a number between 1 and %d, or all", len(groups))
		}
		return stopProcessGroups(groups[n-1 : n])
	}
	if len(groups) == 0 {
		fmt.Println(formatNotice("Nothing goat started is running in the background."))
		return nil
	}

	options := make([]selectOption, 0, len(groups)+1)
	for _, group := range groups {
		options = append(options, selectOption{label: processLabel(group), hint: processHint(group)})
	}
	if len(groups) > 1 {
		options = append(options, selectOption{label: "Stop all", hint: plural(len(groups), "command")})
	}
	chosen, err := selectOptionInteractive(ctx, c, "Running in the background "+style.Glyphs().Dot+" Enter stops one, Esc keeps them", options)
	switch {
	case errors.Is(err, errSelectorUnavailable):
		printProcessGroups(groups)
		return nil
	case err != nil:
		return err
	case chosen < 0:
		return nil
	case chosen == len(groups):
		return stopProcessGroups(groups)
	default:
		return stopProcessGroups(groups[chosen : chosen+1])
	}
}

// printProcessGroups lists the groups with the processes left in each, numbered for
// /ps stop.
func printProcessGroups(groups []workspace.BackgroundGroup) {
	fmt.Println(style.Strong.Render("Running in the background:"))
	for i, group := range groups {
		fmt.Printf("%d. %s %s\n", i+1, processLabel(group), style.Muted.Render("("+processHint(group)+")"))
		processes, err := workspace.GroupProcesses(group.PGID)
		if err != nil {
			fmt.Println(style.Muted.Render("   (its processes cannot be listed: " + err.Error() + ")"))
			continue
		}
		for _, process := range processes {
			fmt.Println(style.Muted.Render(fmt.Sprintf("   %d  %s  %s", process.PID, process.Elapsed, escapeControlChars(truncateForLog(process.Command, 100)))))
		}
	}
	fmt.Println(style.Muted.Render("/ps stop <n> stops one; /ps stop all stops them all."))
}

func processLabel(group workspace.BackgroundGroup) string {
	return escapeControlChars(truncateForLog(strings.Join(strings.Fields(group.Command), " "), 80))
}

func processHint(group workspace.BackgroundGroup) string {
	hint := fmt.Sprintf("group %d %s started %s ago", group.PGID, style.Glyphs().Dot, formatElapsed(time.Since(group.Started)))
	if group.Dir != "." {
		hint += " in " + group.Dir
	}
	return hint
}

func stopProcessGroups(groups []workspace.BackgroundGroup) error {
	var failed []string
	for _, group := range groups {
		if err := workspace.StopBackgroundGroup(group.PGID); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", processLabel(group), err))
			continue
		}
		fmt.Println(formatSuccess("Stopped " + processLabel(group)))
	}
	if len(failed) > 0 {
		return errors.New("could not stop " + strings.Join(failed, "; "))
	}
	return nil
}

// backgroundFooterPart says how many commands goat left running, for the footer under
// the prompt, or "" when there are none.
func backgroundFooterPart() string {
	if n := len(workspace.BackgroundGroups()); n > 0 {
		return plural(n, "command") + " running (/ps)"
	}
	return ""
}

// stopBackgroundOnExit stops what run_bash commands left running, as goat exits, and
// says what it stopped. Left behind, a dev server would keep its port with nothing
// listing it.
func stopBackgroundOnExit(out io.Writer) {
	stopped := workspace.StopAllBackground()
	if len(stopped) == 0 {
		return
	}
	labels := make([]string, len(stopped))
	for i, group := range stopped {
		labels[i] = processLabel(group)
	}
	fmt.Fprintln(out, formatNotice("Stopped what goat left running: "+strings.Join(labels, ", ")))
}

// stopBackgroundOnSignal stops the background commands when goat is closed by a hangup,
// such as its terminal window closing, or by SIGTERM; their sessions are detached from
// the terminal, so nothing else would.
func stopBackgroundOnSignal() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		received := <-signals
		workspace.StopAllBackground()
		code := 128 + 1
		if received == syscall.SIGTERM {
			code = 128 + 15
		}
		os.Exit(code)
	}()
}
