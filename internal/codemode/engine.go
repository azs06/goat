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

	"github.com/dlclark/regexp2/v2"
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
	// MaxResultLines is the most lines a result and its logs should take; a value that
	// would take more is returned as compact JSON.
	MaxResultLines int
}

// DefaultLimits are the limits run_code uses.
var DefaultLimits = Limits{
	Timeout:        120 * time.Second,
	MaxCalls:       50,
	MaxCallStack:   1000,
	MaxLogBytes:    64 << 10,
	MaxCodeBytes:   100_000,
	MaxHeapGrowth:  1 << 30,
	MaxResultLines: 400,
}

const (
	// maxJSONDepth is the deepest JSON.parse accepts. goja parses nested values by
	// recursing in Go, and input nested deeply enough overflows the goroutine stack,
	// which kills the process outright.
	maxJSONDepth = 5000
	// regexpTimeout bounds one regular expression match. goja hands patterns with
	// backreferences or lookaround to regexp2, whose backtracking an interrupt cannot
	// stop; a match that runs out of time is treated as no match.
	regexpTimeout = 2 * time.Second
	// stopGrace is how long a stopped script gets to reach its next instruction, where
	// the interrupt takes effect, before Run returns without it.
	stopGrace = time.Second
)

func init() {
	// goat uses regexp2 v2 only through goja, so this bounds nothing else.
	regexp2.DefaultMatchTimeout = regexpTimeout
}

// Result is what a finished script produced.
type Result struct {
	// Value is the returned value as text: a string as it is, anything else as JSON.
	// HasValue is false when the script returned nothing.
	Value    string
	HasValue bool
	// Logs is the captured console output.
	Logs string
	// Calls counts the host calls that ran; Failed counts those that returned an error.
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

// Unwrap returns what stopped the run when it was stopped from outside, such as
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
	src := stripFence(code)
	program, err := compile(&src)
	if err != nil {
		return Result{}, &Error{Message: err.Error()}
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	clock := startClock(limits.Timeout, func() { cancel(errTimeout) })
	defer clock.stop()
	runCtx = context.WithValue(runCtx, clockKey{}, clock)

	vm := goja.New()
	vm.SetMaxCallStackSize(limits.MaxCallStack)
	r := &run{ctx: runCtx, vm: vm, host: host, limits: limits, src: &src, unhandled: map[*goja.Promise]bool{}, callError: map[*goja.Promise]error{}}
	if err := r.install(); err != nil {
		return Result{}, &Error{Message: err.Error()}
	}

	heapDone := make(chan struct{})
	defer close(heapDone)
	go watchHeap(limits.MaxHeapGrowth, heapDone, func() { cancel(errMemory) })

	// The script runs on its own goroutine so that Run can return once the script is
	// stopped, even when it is inside a long native operation that never reaches an
	// instruction where the interrupt takes effect.
	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := r.execute(program)
		finished <- outcome{result, err}
	}()

	select {
	case out := <-finished:
		return out.result, out.err
	case <-runCtx.Done():
	}
	vm.Interrupt(context.Cause(runCtx))
	grace := time.NewTimer(stopGrace)
	defer grace.Stop()
	for {
		select {
		case out := <-finished:
			return out.result, out.err
		case <-grace.C:
			// A host call in flight is waited for: it has runCtx and returns soon, and the
			// script stops at the instruction after it. Otherwise the script is stuck in
			// native code. It is left to finish on its own; it cannot make another call,
			// since runCtx is done.
			if !r.inHostCall() {
				return r.result(), r.stopped()
			}
			grace.Reset(100 * time.Millisecond)
		}
	}
}

// run is the state of one script. Only the script's goroutine uses the VM; mu guards
// what Run reads from its own goroutine when it stops waiting for the script.
type run struct {
	ctx    context.Context
	vm     *goja.Runtime
	host   Host
	limits Limits
	src    *source

	// stringify is JSON.stringify as it was before the script ran.
	stringify goja.Callable

	mu            sync.Mutex
	logs          strings.Builder
	logsTruncated bool
	calls, failed int
	inHost        bool

	// unhandled holds rejected promises nothing has handled yet, and callError the
	// error of each goat call that rejected.
	unhandled map[*goja.Promise]bool
	callError map[*goja.Promise]error
}

func (r *run) result() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	logs := strings.TrimRight(r.logs.String(), "\n")
	if r.logsTruncated {
		logs += "\n[... further console output dropped ...]"
	}
	return Result{Logs: logs, Calls: r.calls, Failed: r.failed}
}

func (r *run) inHostCall() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inHost
}

// execute runs the program and reads its outcome, on the script's goroutine. Reading
// the outcome can run script code too (getters, toString, toJSON), so it is guarded:
// nothing the script does can panic out of here.
func (r *run) execute(program *goja.Program) (result Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			result, err = r.result(), &Error{Message: fmt.Sprintf("internal error while running the script: %v", p), Result: r.result()}
		}
	}()

	completion, err := r.vm.RunProgram(program)
	if err == nil && r.ctx.Err() != nil {
		// The script caught the error of a call the stop cut short and finished before
		// the interrupt reached it; it was still stopped.
		return r.result(), r.stopped()
	}
	if err != nil {
		return r.result(), r.failure(err)
	}

	var value goja.Value
	var outcome error
	if err := r.guard(func() {
		promise, ok := completion.Export().(*goja.Promise)
		if !ok {
			outcome = &Error{Message: "the script did not produce a promise"}
			return
		}
		switch promise.State() {
		case goja.PromiseStateRejected:
			outcome = r.rejection(promise.Result())
		case goja.PromiseStatePending:
			outcome = &Error{Message: "the script awaited a promise that never settles; run_code has no timers or event loop"}
		default:
			value = promise.Result()
			outcome = r.unawaitedFailure()
		}
	}); err != nil {
		return r.result(), r.failure(err)
	}
	if outcome != nil {
		return r.result(), r.failure(outcome)
	}

	result = r.result()
	if !goja.IsUndefined(value) && !goja.IsNull(value) {
		text, err := r.format(value, result)
		if err != nil {
			return r.result(), r.failure(err)
		}
		result.Value, result.HasValue = text, true
	}
	return result, nil
}

// guard runs f, which calls into the VM from Go, and returns what it threw: a JS
// exception, an interrupt or a stack overflow.
func (r *run) guard(f func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			var interrupted *goja.InterruptedError
			var overflow *goja.StackOverflowError
			if e, ok := p.(error); ok && (errors.As(e, &interrupted) || errors.As(e, &overflow)) {
				err = e
				return
			}
			panic(p)
		}
	}()
	if exception := r.vm.Try(f); exception != nil {
		return exception
	}
	return nil
}

// install adds the goat object and console to the global scope, bounds JSON.parse, and
// keeps the JSON.stringify results are formatted with, before the script can change it.
func (r *run) install() error {
	json := r.vm.Get("JSON").ToObject(r.vm)
	stringify, ok := goja.AssertFunction(json.Get("stringify"))
	if !ok {
		return errors.New("JSON.stringify is not a function")
	}
	r.stringify = stringify
	parse, ok := goja.AssertFunction(json.Get("parse"))
	if !ok {
		return errors.New("JSON.parse is not a function")
	}
	if err := json.Set("parse", r.boundedParse(parse)); err != nil {
		return err
	}

	r.vm.SetPromiseRejectionTracker(func(p *goja.Promise, op goja.PromiseRejectionOperation) {
		switch op {
		case goja.PromiseRejectionReject:
			r.unhandled[p] = true
		case goja.PromiseRejectionHandle:
			delete(r.unhandled, p)
		}
	})

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
// time, in the order the script makes them. Only calls that reach the host count.
func (r *run) hostFunction(name string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := r.vm.NewPromise()
		rejectWith := func(err error) goja.Value {
			r.callError[promise] = err
			if rejectErr := reject(r.vm.NewGoError(err)); rejectErr != nil {
				panic(rejectErr)
			}
			return r.vm.ToValue(promise)
		}

		args, err := r.argumentJSON(name, call.Argument(0))
		if err != nil {
			return rejectWith(err)
		}

		r.mu.Lock()
		if r.ctx.Err() != nil {
			// The run was stopped, so nothing more runs. The interrupt stops the script at
			// its next instruction, whatever it catches.
			r.mu.Unlock()
			r.vm.Interrupt(context.Cause(r.ctx))
			return rejectWith(fmt.Errorf("goat.%s: the script was stopped", name))
		}
		if r.calls >= r.limits.MaxCalls {
			r.mu.Unlock()
			return rejectWith(fmt.Errorf("goat.%s: run_code allows at most %d goat calls per run", name, r.limits.MaxCalls))
		}
		r.calls++
		r.inHost = true
		r.mu.Unlock()

		output, err := r.host.Call(r.ctx, name, args)

		r.mu.Lock()
		r.inHost = false
		if err != nil {
			r.failed++
		}
		r.mu.Unlock()
		if err != nil {
			return rejectWith(err)
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
	text, err := r.json(arg, false)
	if err != nil {
		return "", fmt.Errorf("goat.%s: the argument cannot be converted to JSON: %v", name, err)
	}
	return text, nil
}

// boundedParse wraps JSON.parse to refuse input nested deeper than maxJSONDepth.
func (r *run) boundedParse(parse goja.Callable) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		if depth := jsonDepth(call.Argument(0).String()); depth > maxJSONDepth {
			panic(r.vm.NewTypeError("JSON.parse: the input is nested %d levels deep; at most %d are accepted", depth, maxJSONDepth))
		}
		value, err := parse(call.This, call.Arguments...)
		if err != nil {
			panic(err)
		}
		return value
	}
}

// jsonDepth is the deepest nesting of arrays and objects in JSON text, outside strings.
func jsonDepth(text string) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		}
	}
	return deepest
}

func (r *run) consoleMethod(prefix string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		r.mu.Lock()
		truncated := r.logsTruncated
		r.mu.Unlock()
		if truncated {
			return goja.Undefined()
		}
		parts := make([]string, len(call.Arguments))
		for i, arg := range call.Arguments {
			if s, ok := arg.Export().(string); ok {
				parts[i] = s
			} else if text, err := r.json(arg, false); err == nil && text != "" {
				parts[i] = text
			} else {
				parts[i] = arg.String()
			}
		}
		line := prefix + strings.Join(parts, " ") + "\n"
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.logs.Len()+len(line) > r.limits.MaxLogBytes {
			r.logsTruncated = true
		} else {
			r.logs.WriteString(line)
		}
		return goja.Undefined()
	}
}

// format turns the script's result into text. Objects are pretty-printed, unless that
// and the logs would take more than MaxResultLines; then they are compact.
func (r *run) format(value goja.Value, sofar Result) (string, error) {
	var text string
	err := r.guard(func() {
		if s, ok := value.Export().(string); ok {
			text = s
			return
		}
		pretty, err := r.json(value, true)
		if err != nil {
			var interrupted *goja.InterruptedError
			if errors.As(err, &interrupted) {
				panic(err)
			}
			text = r.printable(value)
			return
		}
		if pretty == "" {
			text = r.printable(value)
			return
		}
		// The call count, "Result:" and "Console output:" take a line each.
		lines := strings.Count(pretty, "\n") + 1 + 3
		if sofar.Logs != "" {
			lines += strings.Count(sofar.Logs, "\n") + 1
		}
		if lines > r.limits.MaxResultLines {
			if compact, err := r.json(value, false); err == nil {
				text = compact
				return
			}
		}
		text = pretty
	})
	return text, err
}

// printable converts a value to a string, or describes it when that throws.
func (r *run) printable(value goja.Value) string {
	var text string
	if r.guard(func() { text = value.String() }) != nil {
		return "[a value that cannot be converted to text]"
	}
	return text
}

// json runs the original JSON.stringify. It returns "" for values JSON leaves out, such
// as functions.
func (r *run) json(value goja.Value, pretty bool) (string, error) {
	args := []goja.Value{value}
	if pretty {
		args = append(args, goja.Null(), r.vm.ToValue(2))
	}
	out, err := r.stringify(goja.Undefined(), args...)
	if err != nil {
		return "", err
	}
	if goja.IsUndefined(out) {
		return "", nil
	}
	return out.String(), nil
}

// unawaitedFailure reports a goat call that failed while nothing awaited or handled
// it, which would otherwise go unnoticed.
func (r *run) unawaitedFailure() error {
	for promise := range r.unhandled {
		if err, ok := r.callError[promise]; ok {
			return &Error{Message: fmt.Sprintf("a goat call failed and nothing awaited it: %v", err)}
		}
	}
	return nil
}

// stopped describes a run stopped from outside: by its timeout, the memory watchdog,
// or the caller's context, whose cause is kept.
func (r *run) stopped() error {
	result := r.result()
	cause := context.Cause(r.ctx)
	switch {
	case errors.Is(cause, errTimeout):
		return &Error{Message: fmt.Sprintf("the script timed out after %s of run time", r.limits.Timeout), Result: result}
	case errors.Is(cause, errMemory):
		return &Error{Message: fmt.Sprintf("the script was stopped: memory grew by more than %d MiB", r.limits.MaxHeapGrowth>>20), Result: result}
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		return &Error{Message: "the script was interrupted", Result: result, cause: cause}
	case cause != nil:
		return &Error{Message: "the script was stopped: " + cause.Error(), Result: result, cause: cause}
	}
	return &Error{Message: "the script was interrupted", Result: result}
}

// failure describes what ended the script: an interrupt, a stack overflow, an
// exception, or an *Error already worded, which gets the latest counts.
func (r *run) failure(err error) error {
	var runErr *Error
	if errors.As(err, &runErr) {
		runErr.Result = r.result()
		return runErr
	}
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) || r.ctx.Err() != nil {
		return r.stopped()
	}
	var overflow *goja.StackOverflowError
	if errors.As(err, &overflow) {
		return &Error{Message: fmt.Sprintf("the call stack grew past %d frames; look for unbounded recursion", r.limits.MaxCallStack), Result: r.result()}
	}
	var exception *goja.Exception
	if errors.As(err, &exception) {
		return r.rejection(exception.Value())
	}
	return &Error{Message: err.Error(), Result: r.result()}
}

// rejection describes the value an uncaught exception or rejected promise carried,
// with the position in the model's code it came from.
func (r *run) rejection(reason goja.Value) error {
	if r.ctx.Err() != nil {
		// The run was stopped while a host call was in flight; that call's error is a
		// symptom, not the cause.
		return r.stopped()
	}
	// A rejected goat call is a GoError; its message already names the tool.
	message := strings.TrimPrefix(r.printable(reason), "GoError: ")
	var position string
	if object, ok := reason.(*goja.Object); ok {
		_ = r.guard(func() {
			if stack := object.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
				position = r.src.errorPosition(stack.String())
			}
		})
	}
	if position != "" {
		return &Error{Message: "uncaught error at " + position + ": " + message, Result: r.result()}
	}
	return &Error{Message: "uncaught error: " + message, Result: r.result()}
}

// watchHeap calls stop once the Go heap grows by more than limit, checking four times
// a second until done is closed. goja has no memory limit of its own, so this watches
// the whole process; it is a backstop against a runaway script, not a quota.
func watchHeap(limit uint64, done <-chan struct{}, stop func()) {
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
		case <-done:
			return
		case <-ticker.C:
			if now := read(); now > start && now-start > limit {
				stop()
				return
			}
		}
	}
}
