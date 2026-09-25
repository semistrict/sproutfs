package sim

import (
	"context"
	"fmt"
	"strconv"
)

type taskKey struct{}

// WithTask identifies a logical caller before it forks concurrent work. Child
// operations inherit the identity through context, including asynchronous
// checkpoints using context.WithoutCancel. It is not a goroutine ID or a
// recorded replay. Sibling tasks must have different names; reuse within one
// sequential task is counted per adapter resource. Without a scheduler the
// identity has no effect.
//
// The identity lives here rather than in platform because only a simulation
// reads it: the real adapters admit nothing and never ask who is calling.
func WithTask(ctx context.Context, name string) context.Context {
	if name == "" {
		panic("empty task name")
	}
	return context.WithValue(ctx, taskKey{}, taskName(ctx)+"/"+strconv.Quote(name))
}

// taskName is the nested identity WithTask has built on this context, empty
// where no caller named itself.
func taskName(ctx context.Context) string {
	name, _ := ctx.Value(taskKey{}).(string)
	return name
}

// Admit chooses which logical caller consumes a shared adapter sequence. I/O
// completion scheduling alone cannot distinguish two concurrent identical RPCs
// whose callers will do different work when their replies arrive.
func (r *Runtime) Admit(ctx context.Context, resource string) error {
	if r.wait == nil {
		return nil
	}
	task := taskName(ctx)
	if task == "" {
		return nil
	}
	key := fmt.Sprintf("task/%s/%s", task, resource)
	r.mu.Lock()
	if r.taskSequences == nil {
		r.taskSequences = make(map[string]uint64)
	}
	r.taskSequences[key]++
	sequence := r.taskSequences[key]
	r.mu.Unlock()
	return r.wait(ctx, fmt.Sprintf("%s/%d", key, sequence), 0, 0)
}
