// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package client

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/leaktest"
	"github.com/cockroachdb/cockroach/util/uuid"
	"github.com/gogo/protobuf/proto"
)

var (
	testKey     = roachpb.Key("a")
	testTS      = roachpb.Timestamp{WallTime: 1, Logical: 1}
	testPutResp = &roachpb.PutResponse{ResponseHeader: roachpb.ResponseHeader{Timestamp: testTS}}
)

func newDB(sender Sender) *DB {
	return &DB{
		sender:          sender,
		txnRetryOptions: DefaultTxnRetryOptions,
	}
}

// TestSender mocks out some of the txn coordinator sender's
// functionality. It responds to PutRequests using testPutResp.
func newTestSender(pre, post func(roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error)) SenderFunc {
	txnKey := roachpb.Key("test-txn")
	txnID := []byte(uuid.NewUUID4())

	return func(_ context.Context, ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		ba.UserPriority = proto.Int32(-1)
		if ba.Txn != nil && len(ba.Txn.ID) == 0 {
			ba.Txn.Key = txnKey
			ba.Txn.ID = txnID
		}

		var br *roachpb.BatchResponse
		var pErr *roachpb.Error
		if pre != nil {
			br, pErr = pre(ba)
		} else {
			br = ba.CreateReply()
		}
		if pErr != nil {
			return nil, pErr
		}
		var writing bool
		status := roachpb.PENDING
		for i, req := range ba.Requests {
			args := req.GetInner()
			if _, ok := args.(*roachpb.PutRequest); ok {
				if !br.Responses[i].SetValue(proto.Clone(testPutResp).(roachpb.Response)) {
					panic("failed to set put response")
				}
			}
			if roachpb.IsTransactionWrite(args) {
				writing = true
			}
		}
		if args, ok := ba.GetArg(roachpb.EndTransaction); ok {
			et := args.(*roachpb.EndTransactionRequest)
			writing = true
			if et.Commit {
				status = roachpb.COMMITTED
			} else {
				status = roachpb.ABORTED
			}
		}
		br.Txn = proto.Clone(ba.Txn).(*roachpb.Transaction)
		if br.Txn != nil && pErr == nil {
			br.Txn.Writing = writing
			br.Txn.Status = status
		}

		if post != nil {
			br, pErr = post(ba)
		}
		return br, pErr
	}
}

func testPut() roachpb.BatchRequest {
	var ba roachpb.BatchRequest
	ba.Timestamp = testTS
	put := &roachpb.PutRequest{}
	put.Key = testKey
	ba.Add(put)
	return ba
}

// TestTxnRequestTxnTimestamp verifies response txn timestamp is
// always upgraded on successive requests.
func TestTxnRequestTxnTimestamp(t *testing.T) {
	defer leaktest.AfterTest(t)
	makeTS := func(walltime int64, logical int32) roachpb.Timestamp {
		return roachpb.ZeroTimestamp.Add(walltime, logical)
	}
	ba := testPut()

	testCases := []struct {
		expRequestTS, responseTS roachpb.Timestamp
	}{
		{makeTS(0, 0), makeTS(10, 0)},
		{makeTS(10, 0), makeTS(10, 1)},
		{makeTS(10, 1), makeTS(10, 0)},
		{makeTS(10, 1), makeTS(20, 1)},
		{makeTS(20, 1), makeTS(20, 1)},
		{makeTS(20, 1), makeTS(0, 0)},
		{makeTS(20, 1), makeTS(20, 1)},
	}

	var testIdx int
	db := NewDB(newTestSender(nil, func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		test := testCases[testIdx]
		if !test.expRequestTS.Equal(ba.Txn.Timestamp) {
			return nil, roachpb.NewError(util.Errorf("%d: expected ts %s got %s", testIdx, test.expRequestTS, ba.Txn.Timestamp))
		}
		br := &roachpb.BatchResponse{}
		br.Txn = &roachpb.Transaction{}
		br.Txn.Update(ba.Txn) // copy
		br.Txn.Timestamp = test.responseTS
		return br, nil
	}))

	txn := NewTxn(*db)

	for testIdx = range testCases {
		if _, pErr := txn.db.sender.Send(context.Background(), ba); pErr != nil {
			t.Fatal(pErr)
		}
	}
}

// TestTxnResetTxnOnAbort verifies transaction is reset on abort.
func TestTxnResetTxnOnAbort(t *testing.T) {
	defer leaktest.AfterTest(t)
	db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		return nil, roachpb.NewError(&roachpb.TransactionAbortedError{
			Txn: *proto.Clone(ba.Txn).(*roachpb.Transaction),
		})
	}, nil))

	txn := NewTxn(*db)
	_, pErr := txn.db.sender.Send(context.Background(), testPut())
	if _, ok := pErr.GoError().(*roachpb.TransactionAbortedError); !ok {
		t.Fatalf("expected TransactionAbortedError, got %v", pErr)
	}

	if len(txn.Proto.ID) != 0 {
		t.Errorf("expected txn to be cleared")
	}
}

// TestTransactionConfig verifies the proper unwrapping and
// re-wrapping of the client's sender when starting a transaction.
// Also verifies that the UserPriority is propagated to the
// transactional client.
func TestTransactionConfig(t *testing.T) {
	defer leaktest.AfterTest(t)
	db := NewDB(newTestSender(nil, nil))
	db.userPriority = 101
	if err := db.Txn(func(txn *Txn) error {
		if txn.db.userPriority != db.userPriority {
			t.Errorf("expected txn user priority %d; got %d", db.userPriority, txn.db.userPriority)
		}
		return nil
	}); err != nil {
		t.Errorf("unexpected error on commit: %s", err)
	}
}

// TestCommitReadOnlyTransaction verifies that transaction is
// committed but EndTransaction is not sent if only read-only
// operations were performed.
func TestCommitReadOnlyTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)
	var calls []roachpb.Method
	db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		calls = append(calls, ba.Methods()...)
		return ba.CreateReply(), nil
	}, nil))
	if err := db.Txn(func(txn *Txn) error {
		_, err := txn.Get("a")
		return err
	}); err != nil {
		t.Errorf("unexpected error on commit: %s", err)
	}
	expectedCalls := []roachpb.Method{roachpb.Get}
	if !reflect.DeepEqual(expectedCalls, calls) {
		t.Errorf("expected %s, got %s", expectedCalls, calls)
	}
}

// TestCommitReadOnlyTransactionExplicit verifies that a read-only
// transaction with an explicit EndTransaction call does not send
// that call.
func TestCommitReadOnlyTransactionExplicit(t *testing.T) {
	defer leaktest.AfterTest(t)
	for _, withGet := range []bool{true, false} {
		var calls []roachpb.Method
		db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
			calls = append(calls, ba.Methods()...)
			return ba.CreateReply(), nil
		}, nil))
		if err := db.Txn(func(txn *Txn) error {
			b := &Batch{}
			if withGet {
				b.Get("foo")
			}
			return txn.CommitInBatch(b)
		}); err != nil {
			t.Errorf("unexpected error on commit: %s", err)
		}
		expectedCalls := []roachpb.Method(nil)
		if withGet {
			expectedCalls = append(expectedCalls, roachpb.Get)
		}
		if !reflect.DeepEqual(expectedCalls, calls) {
			t.Errorf("expected %s, got %s", expectedCalls, calls)
		}
	}
}

// TestCommitMutatingTransaction verifies that transaction is committed
// upon successful invocation of the retryable func.
func TestCommitMutatingTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)

	var calls []roachpb.Method
	db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		calls = append(calls, ba.Methods()...)
		if bt, ok := ba.GetArg(roachpb.BeginTransaction); ok && !bt.Header().Key.Equal(roachpb.Key("a")) {
			t.Errorf("expected begin transaction key to be \"a\"; got %s", bt.Header().Key)
		}
		if et, ok := ba.GetArg(roachpb.EndTransaction); ok && !et.(*roachpb.EndTransactionRequest).Commit {
			t.Errorf("expected commit to be true")
		}
		return ba.CreateReply(), nil
	}, nil))

	// Test all transactional write methods.
	testArgs := []struct {
		f         func(txn *Txn) error
		expMethod roachpb.Method
	}{
		{func(txn *Txn) error { return txn.Put("a", "b") }, roachpb.Put},
		{func(txn *Txn) error { return txn.CPut("a", "b", nil) }, roachpb.ConditionalPut},
		{func(txn *Txn) error {
			_, err := txn.Inc("a", 1)
			return err
		}, roachpb.Increment},
		{func(txn *Txn) error { return txn.Del("a") }, roachpb.Delete},
		{func(txn *Txn) error { return txn.DelRange("a", "b") }, roachpb.DeleteRange},
	}
	for i, test := range testArgs {
		calls = []roachpb.Method{}
		if err := db.Txn(func(txn *Txn) error {
			return test.f(txn)
		}); err != nil {
			t.Errorf("%d: unexpected error on commit: %s", i, err)
		}
		expectedCalls := []roachpb.Method{roachpb.BeginTransaction, test.expMethod, roachpb.EndTransaction}
		if !reflect.DeepEqual(expectedCalls, calls) {
			t.Errorf("%d: expected %s, got %s", i, expectedCalls, calls)
		}
	}
}

// TestTxnInsertBeginTransaction verifies that a begin transaction
// request is inserted just before the first mutating command.
func TestTxnInsertBeginTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)
	var calls []roachpb.Method
	db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		calls = append(calls, ba.Methods()...)
		return ba.CreateReply(), nil
	}, nil))
	if err := db.Txn(func(txn *Txn) error {
		if _, err := txn.Get("foo"); err != nil {
			return err
		}
		return txn.Put("a", "b")
	}); err != nil {
		t.Errorf("unexpected error on commit: %s", err)
	}
	expectedCalls := []roachpb.Method{roachpb.Get, roachpb.BeginTransaction, roachpb.Put, roachpb.EndTransaction}
	if !reflect.DeepEqual(expectedCalls, calls) {
		t.Errorf("expected %s, got %s", expectedCalls, calls)
	}
}

// TestCommitTransactionOnce verifies that if the transaction is
// ended explicitly in the retryable func, it is not automatically
// ended a second time at completion of retryable func.
func TestCommitTransactionOnce(t *testing.T) {
	defer leaktest.AfterTest(t)
	count := 0
	db := NewDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		count++
		return ba.CreateReply(), nil
	}, nil))
	if err := db.Txn(func(txn *Txn) error {
		b := &Batch{}
		b.Put("z", "adding a write exposed a bug in #1882")
		return txn.CommitInBatch(b)
	}); err != nil {
		t.Errorf("unexpected error on commit: %s", err)
	}
	if count != 1 {
		t.Errorf("expected single Batch, got %d sent calls", count)
	}
}

// TestAbortReadOnlyTransaction verifies that aborting a read-only
// transaction does not prompt an EndTransaction call.
func TestAbortReadOnlyTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)
	db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		if _, ok := ba.GetArg(roachpb.EndTransaction); ok {
			t.Errorf("did not expect EndTransaction")
		}
		return ba.CreateReply(), nil
	}, nil))
	if err := db.Txn(func(txn *Txn) error {
		return errors.New("foo")
	}); err == nil {
		t.Error("expected error on abort")
	}
}

// TestEndWriteRestartReadOnlyTransaction verifies that if
// a transaction writes, then restarts and turns read-only,
// an explicit EndTransaction call is still sent if retry-
// able didn't, regardless of whether there is an error
// or not.
func TestEndWriteRestartReadOnlyTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)
	for _, success := range []bool{true, false} {
		expCalls := []roachpb.Method{roachpb.BeginTransaction, roachpb.Put, roachpb.EndTransaction}
		var calls []roachpb.Method
		db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
			calls = append(calls, ba.Methods()...)
			return ba.CreateReply(), nil
		}, nil))
		ok := false
		if err := db.Txn(func(txn *Txn) error {
			if !ok {
				if err := txn.Put("consider", "phlebas"); err != nil {
					t.Fatal(err)
				}
				ok = true
				return &roachpb.TransactionRetryError{} // immediate txn retry
			}
			if !success {
				return errors.New("aborting on purpose")
			}
			return nil
		}); err == nil != success {
			t.Errorf("expected error: %t, got error: %v", !success, err)
		}
		if !reflect.DeepEqual(expCalls, calls) {
			t.Fatalf("expected %v, got %v", expCalls, calls)
		}
	}
}

// TestAbortMutatingTransaction verifies that transaction is aborted
// upon failed invocation of the retryable func.
func TestAbortMutatingTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)
	var calls []roachpb.Method
	db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		calls = append(calls, ba.Methods()...)
		if et, ok := ba.GetArg(roachpb.EndTransaction); ok && et.(*roachpb.EndTransactionRequest).Commit {
			t.Errorf("expected commit to be false")
		}
		return ba.CreateReply(), nil
	}, nil))

	if err := db.Txn(func(txn *Txn) error {
		if err := txn.Put("a", "b"); err != nil {
			return err
		}
		return errors.New("foo")
	}); err == nil {
		t.Error("expected error on abort")
	}
	expectedCalls := []roachpb.Method{roachpb.BeginTransaction, roachpb.Put, roachpb.EndTransaction}
	if !reflect.DeepEqual(expectedCalls, calls) {
		t.Errorf("expected %s, got %s", expectedCalls, calls)
	}
}

// TestRunTransactionRetryOnErrors verifies that the transaction
// is retried on the correct errors.
func TestRunTransactionRetryOnErrors(t *testing.T) {
	defer leaktest.AfterTest(t)
	testCases := []struct {
		err   error
		retry bool // Expect retry?
	}{
		{&roachpb.ReadWithinUncertaintyIntervalError{}, true},
		{&roachpb.TransactionAbortedError{}, true},
		{&roachpb.TransactionPushError{}, true},
		{&roachpb.TransactionRetryError{}, true},
		{&roachpb.RangeNotFoundError{}, false},
		{&roachpb.RangeKeyMismatchError{}, false},
		{&roachpb.TransactionStatusError{}, false},
	}

	for i, test := range testCases {
		count := 0
		db := newDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {

			if _, ok := ba.GetArg(roachpb.Put); ok {
				count++
				if count == 1 {
					return nil, roachpb.NewError(test.err)
				}
			}
			return ba.CreateReply(), nil
		}, nil))
		db.txnRetryOptions.InitialBackoff = 1 * time.Millisecond
		err := db.Txn(func(txn *Txn) error {
			return txn.Put("a", "b")
		})
		if test.retry {
			if count != 2 {
				t.Errorf("%d: expected one retry; got %d", i, count-1)
			}
			if err != nil {
				t.Errorf("%d: expected success on retry; got %s", i, err)
			}
		} else {
			if count != 1 {
				t.Errorf("%d: expected no retries; got %d", i, count)
			}
			if reflect.TypeOf(err) != reflect.TypeOf(test.err) {
				t.Errorf("%d: expected error of type %T; got %T", i, test.err, err)
			}
		}
	}
}

// TestAbortTransactionOnCommitErrors verifies that non-exec transactions are
// aborted on the correct errors.
func TestAbortTransactionOnCommitErrors(t *testing.T) {
	defer leaktest.AfterTest(t)

	testCases := []struct {
		err   error
		abort bool
	}{
		{&roachpb.ReadWithinUncertaintyIntervalError{}, true},
		{&roachpb.TransactionAbortedError{}, false},
		{&roachpb.TransactionPushError{}, true},
		{&roachpb.TransactionRetryError{}, true},
		{&roachpb.RangeNotFoundError{}, true},
		{&roachpb.RangeKeyMismatchError{}, true},
		{&roachpb.TransactionStatusError{}, true},
	}

	for _, test := range testCases {
		var commit, abort bool
		db := NewDB(newTestSender(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {

			switch t := ba.Requests[0].GetInner().(type) {
			case *roachpb.EndTransactionRequest:
				if t.Commit {
					commit = true
					return nil, roachpb.NewError(test.err)
				}
				abort = true
			}
			return ba.CreateReply(), nil
		}, nil))

		txn := NewTxn(*db)
		_ = txn.Put("a", "b")
		_ = txn.Commit()

		if !commit {
			t.Errorf("%T: failed to find commit", test.err)
		}
		if test.abort && !abort {
			t.Errorf("%T: failed to find abort", test.err)
		} else if !test.abort && abort {
			t.Errorf("%T: found unexpected abort", test.err)
		}
	}
}

// TestTransactionStatus verifies that transactions always have their
// status updated correctly.
func TestTransactionStatus(t *testing.T) {
	defer leaktest.AfterTest(t)

	db := NewDB(newTestSender(nil, nil))
	for _, write := range []bool{true, false} {
		for _, commit := range []bool{true, false} {
			txn := NewTxn(*db)

			if _, err := txn.Get("a"); err != nil {
				t.Fatal(err)
			}
			if write {
				if err := txn.Put("a", "b"); err != nil {
					t.Fatal(err)
				}
			}
			if commit {
				if err := txn.Commit(); err != nil {
					t.Fatal(err)
				}
				if a, e := txn.Proto.Status, roachpb.COMMITTED; a != e {
					t.Errorf("write: %t, commit: %t transaction expected to have status %q but had %q", write, commit, e, a)
				}
			} else {
				if err := txn.Rollback(); err != nil {
					t.Fatal(err)
				}
				if a, e := txn.Proto.Status, roachpb.ABORTED; a != e {
					t.Errorf("write: %t, commit: %t transaction expected to have status %q but had %q", write, commit, e, a)
				}
			}
		}
	}
}
