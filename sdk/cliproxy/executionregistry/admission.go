package executionregistry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var ErrInferencePaused = errors.New("inference is paused for a server update")

type admissionContextKey struct{}
type admissionErrorContextKey struct{}

type admissionLease struct {
	owner    *Admission
	released atomic.Bool
}

// Admission 在同一同步边界暂停新推理并等待已接受工作自然结束
// 等待期限到达后恢复接收，安装阶段由 guard 状态核实后显式恢复
type Admission struct {
	mu        sync.Mutex
	instance  string
	active    int
	attempt   string
	deadline  time.Time
	timer     *time.Timer
	committed bool
	changed   chan struct{}
}

type DrainConfirmation struct {
	AttemptID  string    `json:"attempt_id"`
	InstanceID string    `json:"instance_id"`
	Deadline   time.Time `json:"deadline"`
}

func NewAdmission() *Admission {
	return &Admission{instance: uuid.NewString(), changed: make(chan struct{})}
}

func (a *Admission) InstanceID() string {
	if a == nil {
		return ""
	}
	return a.instance
}

// Acquire 继承外层请求的有效租约，release 可重复调用
func (a *Admission) Acquire(ctx, inherited context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	noop := func() {}
	if a == nil {
		return ctx, noop, nil
	}
	for _, source := range []context.Context{ctx, inherited} {
		if source == nil {
			continue
		}
		if err, _ := source.Value(admissionErrorContextKey{}).(error); err != nil {
			return context.WithValue(ctx, admissionErrorContextKey{}, err), noop, err
		}
		if lease, _ := source.Value(admissionContextKey{}).(*admissionLease); lease != nil &&
			lease.owner == a && !lease.released.Load() {
			return context.WithValue(ctx, admissionContextKey{}, lease), noop, nil
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.attempt != "" {
		return context.WithValue(ctx, admissionErrorContextKey{}, ErrInferencePaused), noop, ErrInferencePaused
	}
	accepted, release := a.newLeaseLocked(ctx)
	return accepted, release, nil
}

// Retain 让已接受的 HTTP 请求把媒体会话租约交给真实会话生命周期
func (a *Admission) Retain(ctx context.Context) (func(), error) {
	if a == nil {
		return func() {}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var lease *admissionLease
	if ctx != nil {
		lease, _ = ctx.Value(admissionContextKey{}).(*admissionLease)
	}
	if lease == nil || lease.owner != a || lease.released.Load() {
		return nil, ErrInferencePaused
	}
	_, release := a.newLeaseLocked(context.Background())
	return release, nil
}

func (a *Admission) newLeaseLocked(ctx context.Context) (context.Context, func()) {
	a.active++
	lease := &admissionLease{owner: a}
	return context.WithValue(ctx, admissionContextKey{}, lease), func() {
		if !lease.released.CompareAndSwap(false, true) {
			return
		}
		a.mu.Lock()
		a.active--
		a.notifyLocked()
		a.mu.Unlock()
	}
}

func (a *Admission) Drain(ctx context.Context, attempt string, deadline time.Time) (DrainConfirmation, error) {
	if a == nil || attempt == "" || !deadline.After(time.Now()) {
		return DrainConfirmation{}, errors.New("invalid inference drain request")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DrainConfirmation{}, err
	}
	a.mu.Lock()
	if a.attempt != "" && (a.attempt != attempt || !a.deadline.Equal(deadline)) {
		a.mu.Unlock()
		return DrainConfirmation{}, errors.New("another inference drain is active")
	}
	if a.attempt == "" {
		a.attempt, a.deadline = attempt, deadline.UTC()
		a.timer = time.AfterFunc(time.Until(deadline), func() { a.resume(attempt, deadline) })
	}
	for {
		if a.attempt != attempt || !a.deadline.Equal(deadline) || !time.Now().Before(deadline) {
			a.mu.Unlock()
			return DrainConfirmation{}, errors.New("inference drain expired or was released")
		}
		if a.active == 0 {
			confirmation := DrainConfirmation{AttemptID: attempt, InstanceID: a.instance, Deadline: a.deadline}
			a.mu.Unlock()
			return confirmation, nil
		}
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			a.resume(attempt, deadline)
			return DrainConfirmation{}, ctx.Err()
		case <-changed:
		}
		a.mu.Lock()
	}
}

func (a *Admission) Confirm(confirmation DrainConfirmation) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.confirmLocked(confirmation)
}

func (a *Admission) confirmLocked(confirmation DrainConfirmation) bool {
	return a.attempt != "" && a.attempt == confirmation.AttemptID && a.instance == confirmation.InstanceID &&
		a.deadline.Equal(confirmation.Deadline) && time.Now().Before(a.deadline) && a.active == 0
}

// Commit 在同一同步边界确认等待完成，并保留安装期间的暂停
func (a *Admission) Commit(confirmation DrainConfirmation) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.confirmLocked(confirmation) {
		return false
	}
	a.committed = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	return true
}

// Hold 在服务监听前恢复持久化安装状态的暂停
func (a *Admission) Hold(attempt string) bool {
	if a == nil || attempt == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.attempt != "" && a.attempt != attempt {
		return false
	}
	a.attempt = attempt
	a.committed = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	return true
}

func (a *Admission) Paused(attempt string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attempt != "" && a.attempt == attempt
}

func (a *Admission) Resume(attempt string) {
	a.resume(attempt, time.Time{})
}

func (a *Admission) resume(attempt string, deadline time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.attempt != attempt || a.attempt == "" ||
		(!deadline.IsZero() && (a.committed || !a.deadline.Equal(deadline))) {
		return
	}
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.attempt = ""
	a.deadline = time.Time{}
	a.committed = false
	a.notifyLocked()
}

func (a *Admission) notifyLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}
