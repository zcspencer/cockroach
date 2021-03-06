// Copyright 2017 The Cockroach Authors.
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
// permissions and limitations under the License.
//
// Author: Nikhil Benesch (nikhil.benesch@gmail.com)

package jobs

import (
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
)

// Job manages logging the progress of long-running system processes, like
// backups and restores, to the system.jobs table.
//
// The Record field can be directly modified before Created is called. Updates to
// the Job field after the job has been created will not be written to the
// database, however, even when calling e.g. Started or Succeeded.
type Job struct {
	db     *client.DB
	ex     sqlutil.InternalExecutor
	id     *int64
	Record Record
	txn    *client.Txn

	mu struct {
		syncutil.Mutex
		payload Payload
	}
}

// Details is a marker interface for job details proto structs.
type Details interface{}

// Record stores the job fields that are not automatically managed by
// Job.
type Record struct {
	Description   string
	Username      string
	DescriptorIDs sqlbase.IDs
	Details       Details
}

// Status represents the status of a job in the system.jobs table.
type Status string

const (
	// StatusPending is for jobs that have been created but on which work has
	// not yet started.
	StatusPending Status = "pending"
	// StatusRunning is for jobs that are currently in progress.
	StatusRunning Status = "running"
	// StatusFailed is for jobs that failed.
	StatusFailed Status = "failed"
	// StatusSucceeded is for jobs that have successfully completed.
	StatusSucceeded Status = "succeeded"
)

// NewJob creates a new Job.
func NewJob(db *client.DB, ex sqlutil.InternalExecutor, record Record) *Job {
	return &Job{
		db:     db,
		ex:     ex,
		Record: record,
	}
}

// GetJob creates a new Job initialized from a previously created
// job id.
func GetJob(
	ctx context.Context, db *client.DB, ex sqlutil.InternalExecutor, id int64,
) (*Job, error) {
	j := &Job{
		db: db,
		ex: ex,
		id: &id,
	}
	if err := j.runInTxn(ctx, func(ctx context.Context, txn *client.Txn) error {
		payload, err := j.retrievePayload(ctx, txn)
		if err != nil {
			return err
		}
		j.Record.Description = payload.Description
		j.Record.Username = payload.Username
		j.Record.DescriptorIDs = payload.DescriptorIDs
		switch d := payload.Details.(type) {
		case *Payload_Backup:
			j.Record.Details = *d.Backup
		case *Payload_Restore:
			j.Record.Details = *d.Restore
		case *Payload_SchemaChange:
			j.Record.Details = *d.SchemaChange
		default:
			return errors.Errorf("Job: unsupported job details type %T", d)
		}
		// Don't need to lock because we're the only one who has a handle on this
		// Job so far.
		j.mu.payload = *payload
		return nil
	}); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Job) runInTxn(
	ctx context.Context, retryable func(context.Context, *client.Txn) error,
) error {
	if j.txn != nil {
		defer func() { j.txn = nil }()
		return j.txn.Exec(ctx, client.TxnExecOptions{AutoRetry: true, AssignTimestampImmediately: true},
			func(ctx context.Context, txn *client.Txn, _ *client.TxnExecOptions) error {
				return retryable(ctx, txn)
			})
	}
	return j.db.Txn(ctx, retryable)
}

// WithTxn sets the transaction that this Job will use for its next operation.
// If the transaction is nil, the Job will create a one-off transaction instead.
// If you use WithTxn, this Job will no longer be threadsafe.
func (j *Job) WithTxn(txn *client.Txn) *Job {
	j.txn = txn
	return j
}

// ID returns the ID of the job that this Job is currently tracking. This will
// be nil if Created has not yet been called.
func (j *Job) ID() *int64 {
	return j.id
}

// Created records the creation of a new job in the system.jobs table and
// remembers the assigned ID of the job in the Job. The job information is read
// from the Record field at the time Created is called.
func (j *Job) Created(ctx context.Context) error {
	payload := &Payload{
		Description:   j.Record.Description,
		Username:      j.Record.Username,
		DescriptorIDs: j.Record.DescriptorIDs,
	}
	switch d := j.Record.Details.(type) {
	case BackupDetails:
		payload.Details = &Payload_Backup{Backup: &d}
	case RestoreDetails:
		payload.Details = &Payload_Restore{Restore: &d}
	case SchemaChangeDetails:
		payload.Details = &Payload_SchemaChange{SchemaChange: &d}
	default:
		return errors.Errorf("Job: unsupported job details type %T", d)
	}
	return j.insert(ctx, payload)
}

// Started marks the tracked job as started.
func (j *Job) Started(ctx context.Context) error {
	return j.update(ctx, StatusRunning, func(payload *Payload) (bool, error) {
		if payload.StartedMicros != 0 {
			// Already started - do nothing.
			return false, nil
		}
		payload.StartedMicros = roundTimestamp(timeutil.Now())
		return true, nil
	})
}

// ProgressedFn is a callback that allows arbitrary modifications to a job's
// details when updating its progress.
type ProgressedFn func(ctx context.Context, details interface{})

// Noop is a nil ProgressedFn.
var Noop ProgressedFn

// Progressed updates the progress of the tracked job to fractionCompleted. A
// fractionCompleted that is less than the currently-recorded fractionCompleted
// will be silently ignored. If progressedFn is non-nil, it will be invoked with
// a pointer to the job's details to allow for modifications to the details
// before the job is saved. If no such modifications are required, pass Noop
// instead of nil for readability.
func (j *Job) Progressed(
	ctx context.Context, fractionCompleted float32, progressedFn ProgressedFn,
) error {
	if fractionCompleted < 0.0 || fractionCompleted > 1.0 {
		return errors.Errorf(
			"Job: fractionCompleted %f is outside allowable range [0.0, 1.0] (job %d)",
			fractionCompleted, j.id,
		)
	}
	return j.update(ctx, StatusRunning, func(payload *Payload) (bool, error) {
		if payload.StartedMicros == 0 {
			return false, errors.Errorf("Job: job %d not started", j.id)
		}
		if payload.FinishedMicros != 0 {
			return false, errors.Errorf("Job: job %d already finished", j.id)
		}
		if fractionCompleted > payload.FractionCompleted {
			payload.FractionCompleted = fractionCompleted
		}
		if progressedFn != nil {
			progressedFn(ctx, payload.Details)
		}
		return true, nil
	})
}

// Failed marks the tracked job as having failed with the given error. Any
// errors encountered while updating the jobs table are logged but not returned,
// under the assumption that the the caller is already handling a more important
// error and doesn't care about this one.
func (j *Job) Failed(ctx context.Context, err error) {
	// To simplify cleanup routines, it is not an error to call Failed on a job
	// that was never Created.
	if j.id == nil {
		return
	}
	internalErr := j.update(ctx, StatusFailed, func(payload *Payload) (bool, error) {
		if payload.FinishedMicros != 0 {
			// Already finished - do nothing.
			return false, nil
		}
		payload.Error = err.Error()
		payload.FinishedMicros = roundTimestamp(timeutil.Now())
		return true, nil
	})
	if internalErr != nil {
		log.Errorf(ctx, "Job: ignoring error %v while logging failure for job %d: %+v",
			err, j.id, internalErr)
	}
}

// Succeeded marks the tracked job as having succeeded and sets its fraction
// completed to 1.0.
func (j *Job) Succeeded(ctx context.Context) error {
	return j.update(ctx, StatusSucceeded, func(payload *Payload) (bool, error) {
		if payload.FinishedMicros != 0 {
			// Already finished - do nothing.
			return false, nil
		}
		payload.FinishedMicros = roundTimestamp(timeutil.Now())
		payload.FractionCompleted = 1.0
		return true, nil
	})
}

// Payload returns the most recently sent Payload for this Job. Will return an
// empty Payload until Created() is called on a new Job.
func (j *Job) Payload() Payload {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.mu.payload
}

func (j *Job) insert(ctx context.Context, payload *Payload) error {
	if j.id != nil {
		// Already created - do nothing.
		return nil
	}

	var row parser.Datums
	if err := j.runInTxn(ctx, func(ctx context.Context, txn *client.Txn) error {
		payload.ModifiedMicros = roundTimestamp(txn.Proto().OrigTimestamp.GoTime())
		payloadBytes, err := protoutil.Marshal(payload)
		if err != nil {
			return err
		}

		const stmt = "INSERT INTO system.jobs (status, payload) VALUES ($1, $2) RETURNING id"
		row, err = j.ex.QueryRowInTransaction(ctx, "job-insert", txn, stmt, StatusPending, payloadBytes)
		return err
	}); err != nil {
		return err
	}
	j.mu.payload = *payload
	j.id = (*int64)(row[0].(*parser.DInt))

	return nil
}

func (j *Job) retrievePayload(ctx context.Context, txn *client.Txn) (*Payload, error) {
	const selectStmt = "SELECT payload FROM system.jobs WHERE id = $1"
	row, err := j.ex.QueryRowInTransaction(ctx, "log-job", txn, selectStmt, *j.id)
	if err != nil {
		return nil, err
	}

	return UnmarshalPayload(row[0])
}

func (j *Job) update(
	ctx context.Context, newStatus Status, updateFn func(*Payload) (doUpdate bool, err error),
) error {
	if j.id == nil {
		return errors.New("Job: cannot update: job not created")
	}

	var payload *Payload
	if err := j.runInTxn(ctx, func(ctx context.Context, txn *client.Txn) error {
		var err error
		payload, err = j.retrievePayload(ctx, txn)
		if err != nil {
			return err
		}
		doUpdate, err := updateFn(payload)
		if err != nil {
			return err
		}
		if !doUpdate {
			return nil
		}
		payload.ModifiedMicros = roundTimestamp(timeutil.Now())
		payloadBytes, err := protoutil.Marshal(payload)
		if err != nil {
			return err
		}

		const updateStmt = "UPDATE system.jobs SET status = $1, payload = $2 WHERE id = $3"
		n, err := j.ex.ExecuteStatementInTransaction(
			ctx, "job-update", txn, updateStmt, newStatus, payloadBytes, *j.id)
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.Errorf("Job: expected exactly one row affected, but %d rows affected by job update", n)
		}
		return nil
	}); err != nil {
		return err
	}
	if payload != nil {
		j.mu.Lock()
		j.mu.payload = *payload
		j.mu.Unlock()
	}
	return nil
}

// Job types are named for the SQL query that creates them.
const (
	TypeBackup       string = "BACKUP"
	TypeRestore      string = "RESTORE"
	TypeSchemaChange string = "SCHEMA CHANGE"
)

// Typ returns the payload's job type.
func (p *Payload) Typ() string {
	switch p.Details.(type) {
	case *Payload_Backup:
		return TypeBackup
	case *Payload_Restore:
		return TypeRestore
	case *Payload_SchemaChange:
		return TypeSchemaChange
	default:
		panic("Payload.Typ called on a payload with an unknown details type")
	}
}

// UnmarshalPayload unmarshals and returns the Payload encoded in the
// input datum, which should be a DBytes.
func UnmarshalPayload(datum parser.Datum) (*Payload, error) {
	payload := &Payload{}
	bytes, ok := datum.(*parser.DBytes)
	if !ok {
		return nil, errors.Errorf(
			"Job: failed to unmarshal payload as DBytes (was %T)", bytes)
	}
	if err := proto.Unmarshal([]byte(*bytes), payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// TIMESTAMP columns round to the nearest microsecond, so we replicate that
// behavior for our protobuf fields. Naive truncation can lead to anomalies
// where jobs are started before they're created.
func roundTimestamp(ts time.Time) int64 {
	return ts.Round(time.Microsecond).UnixNano() / time.Microsecond.Nanoseconds()
}

var _ Details = BackupDetails{}
var _ Details = RestoreDetails{}
var _ Details = SchemaChangeDetails{}
