// Package codemode runs JavaScript the model writes against goat's tools. The script
// calls each tool as an async function on a global goat object; the host runs every
// call itself, so the script can reach nothing goat does not install. Only the value
// the script returns and what it logs go back to the model.
package codemode

import (
	"context"
	"errors"
	"fmt"
	"runtime/metrics"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Host is what a script can call.
type Host struct {
	// Names are the functions installed on the goat object.
	Names []string
	// Call runs one function with its argument as a JSON object. ctx is cancelled when
	// the run is interrupted or times out.
	Call func(ctx context.Context, name, argsJSON string) (string, error)
}

// Limits bound one run.
type Limits struct {
	// Timeout is the run time allowed, not counting time PauseClock excludes.
	Timeout time.Duration
	// MaxCalls is the number of host calls a run may make.
	MaxCalls int
	// MaxCallStack is the deepest JavaScript call stack allowed.
	MaxCallStack int
	// MaxLogBytes is how much console output is kept.
	MaxLogBytes int
	// MaxCodeBytes is the longest script accepted.
	MaxCodeBytes int
	// MaxHeapGrowth is how far the Go heap may grow during a run before it is stopped.
	MaxHeapGrowth uint64
}

// DefaultLimits are the limits run_code uses.
var DefaultLimits = Limits{
	Timeout:       120 * time.Second,
	MaxCalls:      50,
	MaxCallStack:  1000,
	MaxLogBytes:   64 << 10,
	MaxCodeBytes:  100_000,
	MaxHeapGrowth: 1 << 30,
}

// Result is what a finished script produced.
type Result struct {
	// Value is the returned value as text: a string as it is, anything else as JSON.
	// HasValue is false when the script returned nothing.
	Value    string
	HasValue bool
	// Logs is the captured console output.
	Logs string
	// Calls counts host calls; Failed counts those that returned an error.
	Calls, Failed int
}

// Error is a run that failed. The logs and call counts up to the failure are kept,
// because the calls that ran may already have changed things.
type Error struct {
	Message string
	Result
	cause error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns what stopped the run when it was interrupted, such as
// context.Canceled.
func (e *Error) Unwrap() error { return e.cause }

var (
	errTimeout = errors.New("timeout")
	errMemory  = errors.New("memory limit")
)

// Run runs code with the functions host provides.
func Run(ctx context.Context, code string, host Host, limits Limits) (Result, error) {
	if len(code) > limits.MaxCodeBytes {
		return Result{}, &Error{Message: fmt.Sprintf("the code is %d bytes; at most %d are accepted", len(code), limits.MaxCodeBytes)}
	}
	program, err := compile(stripFence(code))
	if err != nil {
		return Result{}, &Error{Message: err.Error()}
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	vm := goja.New()
	vm.SetMaxCallStackSize(limits.MaxCallStack)
	r := &run{vm: vm, host: host, limits: limits}

	// Whatever stops the run cancels runCtx, which interrupts the script and any host
	// call in flight. An interrupt takes effect at the script's next instruction, so a
	// host call has to notice runCtx itself.
	clock := startClock(limits.Timeout, func() { cancel(errTimeout) })
	defer clock.stop()
	runCtx = context.WithValue(runCtx, clockKey{}, clock)
	r.ctx = runCtx
	finished := make(chan struct{})
	var watchers sync.WaitGroup
	defer func() {
		close(finished)
		watchers.Wait()
	}()
	watchers.Add(2)
	go func() {
		defer watchers.Done()
		select {
		case <-runCtx.Done():
			vm.Interrupt(context.Cause(runCtx))
		case <-finished:
		}
	}()
	go func() {
		defer watchers.Done()
		watchHeap(limits.MaxHeapGrowth, finished, func() { cancel(errMemory) })
	}()

	if err := r.install(); err != nil {
		return Result{}, &Error{Message: err.Error()}
	}
	completion, err := vm.RunProgram(program)
	if err != nil {
		return r.result(), r.failure(err)
	}
	promise, ok := completion.Export().(*goja.Promise)
	if !ok {
		return r.result(), &Error{Message: "the script did not produce a promise", Result: r.result()}
	}
	switch promise.State() {
	case goja.PromiseStateRejected:
		return r.result(), r.rejection(promise.Result())
	case goja.PromiseStatePending:
		return r.result(), &Error{Message: "the script awaited a promise that never settles; run_code has no timers or event loop", Result: r.result()}
	}

	result := r.result()
	value := promise.Result()
	if !goja.IsUndefined(value) && !goja.IsNull(value) {
		text, err := r.format(value)
		if err != nil {
			return r.result(), r.failure(err)
		}
		result.Value, result.HasValue = text, true
	}
	return result, nil
}

// run is the state of one script.
type run struct {
	ctx    context.Context
	vm     *goja.Runtime
	host   Host
	limits Limits

	logs          strings.Builder
	logsTruncated bool
	calls, failed int
}

func (r *run) result() Result {
	logs := strings.TrimRight(r.logs.String(), "\n")
	if r.logsTruncated {
		logs += "\n[... further console output dropped ...]"
	}
	return Result{Logs: logs, Calls: r.calls, Failed: r.failed}
}

// install adds the goat object and console to the global scope.
func (r *run) install() error {
	goat := r.vm.NewObject()
	for _, name := range r.host.Names {
		if err := goat.Set(name, r.hostFunction(name)); err != nil {
			return err
		}
	}
	if err := r.vm.Set("goat", goat); err != nil {
		return err
	}

	console := r.vm.NewObject()
	for method, prefix := range map[string]string{"log": "", "info": "", "debug": "", "warn": "[warn] ", "error": "[error] "} {
		if err := console.Set(method, r.consoleMethod(prefix)); err != nil {
			return err
		}
	}
	return r.vm.Set("console", console)
}

// hostFunction is goat.<name>: it runs the call at once and returns a settled promise,
// so scripts can await it, chain .then or pass it to Promise.all. Calls run one at a
// time, in the order the script makes them.
func (r *run) hostFunction(name string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := r.vm.NewPromise()
		settle := func(err error) goja.Value {
			r.failed++
			if rejectErr := reject(r.vm.NewGoError(err)); rejectErr != nil {
				panic(rejectErr)
			}
			return r.vm.ToValue(promise)
		}

		args, err := r.argumentJSON(name, call.Argument(0))
		if err != nil {
			return settle(err)
		}
		if r.calls >= r.limits.MaxCalls {
			r.calls++
			return settle(fmt.Errorf("goat.%s: run_code allows at most %d goat.* calls per run", name, r.limits.MaxCalls))
		}
		r.calls++
		output, err := r.host.Call(r.ctx, name, args)
		if err != nil {
			return settle(err)
		}
		if err := resolve(output); err != nil {
			panic(err)
		}
		return r.vm.ToValue(promise)
	}
}

// argumentJSON converts a call's one argument to a JSON object. A missing argument is
// an empty object.
func (r *run) argumentJSON(name string, arg goja.Value) (string, error) {
	if goja.IsUndefined(arg) || goja.IsNull(arg) {
		return "{}", nil
	}
	object, ok := arg.(*goja.Object)
	if !ok || object.ClassName() == "Array" || object.ClassName() == "Function" {
		return "", fmt.Errorf("goat.%s takes one object argument, such as { path: \"README.md\" }", name)
	}
	text, err := r.stringify(arg, false)
	if err != nil {
		return "", fmt.Errorf("goat.%s: the argument cannot be converted to JSON: %v", name, err)
	}
	return text, nil
}

func (r *run) consoleMethod(prefix string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		if r.logsTruncated {
			return goja.Undefined()
		}
		parts := make([]string, len(call.Arguments))
		for i, arg := range call.Arguments {
			if s, ok := arg.Export().(string); ok {
				parts[i] = s
			} else if text, err := r.stringify(arg, false); err == nil && text != "" {
				parts[i] = text
			} else {
				parts[i] = arg.String()
			}
		}
		line := prefix + strings.Join(parts, " ") + "\n"
		if r.logs.Len()+len(line) > r.limits.MaxLogBytes {
			r.logsTruncated = true
			return goja.Undefined()
		}
		r.logs.WriteString(line)
		return goja.Undefined()
	}
}

// maxPrettyLines is the longest pretty-printed result; longer ones are sent compact.
const maxPrettyLines = 400

// format turns the script's result into text.
func (r *run) format(value goja.Value) (string, error) {
	if s, ok := value.Export().(string); ok {
		return s, nil
	}
	pretty, err := r.stringify(value, true)
	if err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			return "", err
		}
		return value.String(), nil
	}
	if pretty == "" {
		return value.String(), nil
	}
	if strings.Count(pretty, "\n") >= maxPrettyLines {
		if compact, err := r.stringify(value, false); err == nil {
			return compact, nil
		}
	}
	return pretty, nil
}

// stringify runs JSON.stringify in the VM. It returns "" for values JSON leaves out,
// such as functions.
func (r *run) stringify(value goja.Value, pretty bool) (string, error) {
	stringify, ok := goja.AssertFunction(r.vm.Get("JSON").ToObject(r.vm).Get("stringify"))
	if !ok {
		return "", errors.New("JSON.stringify is not a function")
	}
	args := []goja.Value{value}
	if pretty {
		args = append(args, goja.Null(), r.vm.ToValue(2))
	}
	out, err := stringify(goja.Undefined(), args...)
	if err != nil {
		return "", err
	}
	if goja.IsUndefined(out) {
		return "", nil
	}
	return out.String(), nil
}

// failure describes an error that stopped the script outside its promise: an
// interrupt, a stack overflow, or an exception thrown while formatting the result.
func (r *run) failure(err error) error {
	result := r.result()
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		cause := context.Cause(r.ctx)
		switch {
		case errors.Is(cause, errTimeout):
			return &Error{Message: fmt.Sprintf("the script timed out after %s of run time", r.limits.Timeout), Result: result}
		case errors.Is(cause, errMemory):
			return &Error{Message: fmt.Sprintf("the script was stopped: memory grew by more than %d MiB", r.limits.MaxHeapGrowth>>20), Result: result}
		case cause != nil:
			return &Error{Message: "the script was interrupted", Result: result, cause: cause}
		}
		return &Error{Message: "the script was interrupted", Result: result}
	}
	var overflow *goja.StackOverflowError
	if errors.As(err, &overflow) {
		return &Error{Message: fmt.Sprintf("the call stack grew past %d frames; look for unbounded recursion", r.limits.MaxCallStack), Result: result}
	}
	var exception *goja.Exception
	if errors.As(err, &exception) {
		return r.rejection(exception.Value())
	}
	return &Error{Message: err.Error(), Result: result}
}

// rejection describes the value an uncaught exception or rejected promise carried,
// with the position in the model's code it came from.
func (r *run) rejection(reason goja.Value) error {
	result := r.result()
	if r.ctx.Err() != nil {
		// The run was stopped while a host call was in flight; that call's error is a
		// symptom, not the cause.
		return r.failure(&goja.InterruptedError{})
	}
	// A rejected goat call is a GoError; its message already names the tool.
	message := strings.TrimPrefix(reason.String(), "GoError: ")
	if object, ok := reason.(*goja.Object); ok {
		if stack := object.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
			if position := errorPosition(stack.String()); position != "" {
				return &Error{Message: "uncaught error at " + position + ": " + message, Result: result}
			}
		}
	}
	return &Error{Message: "uncaught error: " + message, Result: result}
}

// watchHeap calls stop once the Go heap grows by more than limit, checking four times
// a second until finished is closed. goja has no memory limit of its own, so this
// watches the whole process; it is a backstop against a runaway script, not a quota.
func watchHeap(limit uint64, finished <-chan struct{}, stop func()) {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 {
		metrics.Read(sample)
		if sample[0].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return sample[0].Value.Uint64()
	}
	start := read()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-finished:
			return
		case <-ticker.C:
			if now := read(); now > start && now-start > limit {
				stop()
				return
			}
		}
	}
}
