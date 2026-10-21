package executionregistry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func waitForAdmissionPause(t *testing.T, admission *Admission) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		admission.mu.Lock()
		paused := admission.attempt != ""
		admission.mu.Unlock()
		if paused {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("admission did not pause")
}

func TestAdmissionWaitsForAcceptedWorkAndKeepsItsNestedExecution(t *testing.T) {
	admission := NewAdmission()
	ctx, release, err := admission.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadline := time.Now().Add(time.Second)
	result := make(chan DrainConfirmation, 1)
	failure := make(chan error, 1)
	go func() {
		confirmation, err := admission.Drain(context.Background(), "attempt", deadline)
		if err != nil {
			failure <- err
			return
		}
		result <- confirmation
	}()
	waitForAdmissionPause(t, admission)
	denied, _, err := admission.Acquire(context.Background(), nil)
	if !errors.Is(err, ErrInferencePaused) {
		t.Fatalf("new request accepted during pause: %v", err)
	}
	_, nestedRelease, err := admission.Acquire(context.Background(), ctx)
	if err != nil {
		t.Fatalf("accepted upload lost its execution permission: %v", err)
	}
	nestedRelease()
	select {
	case <-result:
		t.Fatal("drain finished before accepted work ended")
	default:
	}
	release()
	var confirmation DrainConfirmation
	select {
	case confirmation = <-result:
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("drain did not finish")
	}
	if !admission.Confirm(confirmation) || NewAdmission().Confirm(confirmation) {
		t.Fatal("confirmation was not bound to one process instance")
	}
	wrong := confirmation
	wrong.AttemptID = "other-attempt"
	if admission.Confirm(wrong) {
		t.Fatal("another attempt reused the confirmation")
	}
	admission.Resume("other-attempt")
	if !admission.Confirm(confirmation) {
		t.Fatal("another attempt released the pause")
	}
	admission.Resume("attempt")
	if admission.Confirm(confirmation) {
		t.Fatal("released confirmation remained valid")
	}
	if _, _, err := admission.Acquire(denied, nil); !errors.Is(err, ErrInferencePaused) {
		t.Fatal("previously rejected execution became accepted without a new request")
	}
	_, releaseNew, err := admission.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseNew()
}

func TestAdmissionCancellationRestoresAcceptanceWithoutEndingExistingWork(t *testing.T) {
	admission := NewAdmission()
	_, release, _ := admission.Acquire(context.Background(), nil)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := admission.Drain(ctx, "attempt", time.Now().Add(time.Minute))
		result <- err
	}()
	waitForAdmissionPause(t, admission)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result = %v", err)
	}
	_, releaseNew, err := admission.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatalf("cancel did not restore acceptance: %v", err)
	}
	releaseNew()
	admission.mu.Lock()
	active := admission.active
	admission.mu.Unlock()
	if active != 1 {
		t.Fatalf("cancellation changed the original active request: %d", active)
	}
}

func TestAdmissionExpiresAfterGuardStopsAndRetainsMediaUntilItEnds(t *testing.T) {
	admission := NewAdmission()
	ctx, releaseHTTP, _ := admission.Acquire(context.Background(), nil)
	releaseMedia, err := admission.Retain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	releaseHTTP()
	_, err = admission.Drain(context.Background(), "attempt", time.Now().Add(20*time.Millisecond))
	if err == nil {
		t.Fatal("retained media session was ignored")
	}
	_, releaseNew, err := admission.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatalf("expired guard pause was not released: %v", err)
	}
	releaseNew()
	releaseMedia()
	releaseMedia()
	confirmation, err := admission.Drain(context.Background(), "next-attempt", time.Now().Add(20*time.Millisecond))
	if err != nil || !admission.Confirm(confirmation) {
		t.Fatalf("next update drain: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if admission.Confirm(confirmation) {
		t.Fatal("expired confirmation remained valid")
	}
	_, releaseNew, err = admission.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseNew()
}

func TestAdmissionOldDeadlineCannotReleaseANewerPause(t *testing.T) {
	admission := NewAdmission()
	oldDeadline := time.Now().Add(time.Minute)
	if _, err := admission.Drain(context.Background(), "attempt", oldDeadline); err != nil {
		t.Fatal(err)
	}
	admission.Resume("attempt")
	confirmation, err := admission.Drain(context.Background(), "attempt", oldDeadline.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	admission.resume("attempt", oldDeadline)
	if !admission.Confirm(confirmation) {
		t.Fatal("an old timer or cancelled wait released a newer pause")
	}
	admission.Resume("attempt")
}

func TestAdmissionCommitKeepsThePauseUntilExplicitRecovery(t *testing.T) {
	admission := NewAdmission()
	deadline := time.Now().Add(time.Minute)
	confirmation, err := admission.Drain(context.Background(), "attempt", deadline)
	if err != nil {
		t.Fatal(err)
	}
	wrong := confirmation
	wrong.InstanceID = "another-instance"
	if admission.Commit(wrong) || !admission.Commit(confirmation) {
		t.Fatal("install commitment did not validate the process identity")
	}
	admission.resume("attempt", deadline)
	if !admission.Paused("attempt") || admission.Hold("another-attempt") {
		t.Fatal("a timer or another attempt released the installation pause")
	}
	_, release, err := admission.Acquire(context.Background(), nil)
	release()
	if !errors.Is(err, ErrInferencePaused) {
		t.Fatalf("committed installation accepted inference: %v", err)
	}
	admission.Resume("attempt")
	_, release, err = admission.Acquire(context.Background(), nil)
	release()
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewAdmission()
	if !restarted.Hold("attempt") || restarted.Commit(confirmation) {
		t.Fatal("a restarted process reused the previous process confirmation")
	}
	restarted.Resume("another-attempt")
	if !restarted.Paused("attempt") {
		t.Fatal("another attempt released the restored pause")
	}
	restarted.Resume("attempt")
}

func TestAdmissionConcurrentAcceptanceAndPause(t *testing.T) {
	admission := NewAdmission()
	var requests sync.WaitGroup
	for range 100 {
		requests.Go(func() {
			_, release, err := admission.Acquire(context.Background(), nil)
			if err == nil {
				release()
				release()
			}
		})
	}
	confirmation, err := admission.Drain(context.Background(), "attempt", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	requests.Wait()
	if !admission.Confirm(confirmation) {
		t.Fatal("concurrent acceptance escaped the pause boundary")
	}
	admission.Resume("attempt")
}
