package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUserOperationGateRejectsWorkAfterWithdrawalCommits(t *testing.T) {
	gate := NewUserOperationGate()
	userID := uuid.New()

	release, admitted := gate.BeginOperation(userID)
	if !admitted {
		t.Fatal("initial operation was not admitted")
	}
	withdrawalDone := make(chan error, 1)
	go func() {
		owner, err := gate.BeginWithdrawal(context.Background(), userID)
		if err != nil {
			withdrawalDone <- err
			return
		}
		gate.MarkDeleted(userID)
		owner()
		withdrawalDone <- nil
	}()
	release()
	if err := <-withdrawalDone; err != nil {
		t.Fatal(err)
	}

	if _, admitted := gate.BeginOperation(userID); admitted {
		t.Fatal("operation admitted after the withdrawal gate was marked deleted")
	}
	if _, err := gate.BeginWithdrawal(context.Background(), userID); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("second withdrawal error = %v, want ErrUserInactive", err)
	}
}

func TestUserOperationGateIsolatesUsersAndRejectsDuplicateWithdrawal(t *testing.T) {
	gate := NewUserOperationGate()
	targetUserID := uuid.New()
	otherUserID := uuid.New()

	releaseTarget, err := gate.BeginWithdrawal(context.Background(), targetUserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.BeginWithdrawal(context.Background(), targetUserID); !errors.Is(err, ErrWithdrawalInProgress) {
		t.Fatalf("duplicate withdrawal error = %v, want ErrWithdrawalInProgress", err)
	}
	if _, admitted := gate.BeginOperation(targetUserID); admitted {
		t.Fatal("target operation admitted while withdrawal owns the gate")
	}

	releaseOther, err := gate.BeginWithdrawal(context.Background(), otherUserID)
	if err != nil {
		t.Fatalf("other user's withdrawal was blocked: %v", err)
	}
	releaseOther()
	releaseTarget()
}

func TestUserOperationGateCancelsCancellableWorkWhenWithdrawalStarts(t *testing.T) {
	gate := NewUserOperationGate()
	userID := uuid.New()

	transferCtx, releaseTransfer, admitted := gate.BeginCancellableOperation(context.Background(), userID)
	if !admitted {
		t.Fatal("cancellable operation was not admitted")
	}
	releaseWork, admitted := gate.BeginOperation(userID)
	if !admitted {
		t.Fatal("operation was not admitted")
	}
	withdrawalDone := make(chan error, 1)
	go func() {
		owner, err := gate.BeginWithdrawal(context.Background(), userID)
		if err == nil {
			owner()
		}
		withdrawalDone <- err
	}()

	select {
	case <-transferCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("withdrawal did not cancel the cancellable operation")
	}
	releaseTransfer()
	// 일반 작업은 취소하지 않고 끝날 때까지 기다린다.
	select {
	case err := <-withdrawalDone:
		t.Fatalf("withdrawal finished before the ordinary operation ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseWork()
	if err := <-withdrawalDone; err != nil {
		t.Fatal(err)
	}
}

func TestUserOperationGateRejectsCancellableWorkDuringWithdrawal(t *testing.T) {
	gate := NewUserOperationGate()
	userID := uuid.New()

	owner, err := gate.BeginWithdrawal(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	defer owner()
	if _, _, admitted := gate.BeginCancellableOperation(context.Background(), userID); admitted {
		t.Fatal("cancellable operation admitted while withdrawal owns the gate")
	}
}
