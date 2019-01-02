/*
Copyright 2018 github.com/ucirello

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pglock

import (
	"context"
	"database/sql"
	"io/ioutil"
	"log"
	"net"
	"time"

	"cirello.io/errors"
	"github.com/lib/pq"
)

// DefaultTableName defines the table which the client is going to use to store
// the content and the metadata of the locks. Use WithCustomTable to modify this
// value.
const DefaultTableName = "locks"

// DefaultLeaseDuration is the recommended period of time that a lock can be
// considered valid before being stolen by another client. Use WithLeaseDuration
// to modify this value.
const DefaultLeaseDuration = 20 * time.Second

// DefaultHeartbeatFrequency is the recommended frequency that client should
// refresh the lock so to avoid other clients from stealing it. Use
// WithHeartbeatFrequency to modify this value.
const DefaultHeartbeatFrequency = 5 * time.Second

// ErrNotPostgreSQLDriver is returned when an invalid database connection is
// passed to this locker client.
var ErrNotPostgreSQLDriver = errors.E("this is not a PostgreSQL connection")

// ErrNotAcquired indicates the given lock is already enforce to some other
// client.
var ErrNotAcquired = errors.E("cannot acquire lock")

// ErrLockAlreadyReleased indicates that a release call cannot be fulfilled
// because the client does not hold the lock
var ErrLockAlreadyReleased = errors.E("lock is already released")

// ErrLockNotFound is returned for get calls on missing lock entries.
var ErrLockNotFound = errors.E(errors.NotExist, "lock not found")

// Validation errors
var (
	ErrDurationTooSmall = errors.E("Heartbeat period must be no more than half the length of the Lease Duration, " +
		"or locks might expire due to the heartbeat thread taking too long to update them (recommendation is to make it much greater, for example " +
		"4+ times greater)")
)

// Client is the PostgreSQL's backed distributed lock. Make sure it is always
// configured to talk to leaders and not followers in the case of replicated
// setups.
type Client struct {
	db                 *sql.DB
	tableName          string
	leaseDuration      time.Duration
	heartbeatFrequency time.Duration
	log                Logger
}

// New returns a locker client from the given database connection.
func New(db *sql.DB, opts ...ClientOption) (*Client, error) {
	if db == nil {
		return nil, ErrNotPostgreSQLDriver
	} else if _, ok := db.Driver().(*pq.Driver); !ok {
		return nil, ErrNotPostgreSQLDriver
	}
	c := &Client{
		db:                 db,
		tableName:          DefaultTableName,
		leaseDuration:      DefaultLeaseDuration,
		heartbeatFrequency: DefaultHeartbeatFrequency,
		log:                log.New(ioutil.Discard, "", 0),
	}
	for _, opt := range opts {
		opt(c)
	}
	if isDurationTooSmall(c) {
		return nil, ErrDurationTooSmall
	}
	return c, nil
}

func isDurationTooSmall(c *Client) bool {
	return c.heartbeatFrequency > 0 && c.leaseDuration < 2*c.heartbeatFrequency
}

func (c *Client) newLock(name string, opts []LockOption) *Lock {
	l := &Lock{
		client:          c,
		name:            name,
		leaseDuration:   c.leaseDuration,
		heartbeatCancel: func() {},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// CreateTable prepares a PostgreSQL table with the right DDL for it to be used
// by this lock client. If the table already exists, it will return an error.
func (c *Client) CreateTable() error {
	cmds := []string{
		`CREATE TABLE ` + c.tableName + ` (
			name CHARACTER VARYING(255) PRIMARY KEY,
			record_version_number BIGINT,
			data BYTEA
		);`,
		`CREATE SEQUENCE ` + c.tableName + `_rvn OWNED BY ` + c.tableName + `.record_version_number`,
	}
	for _, cmd := range cmds {
		_, err := c.db.Exec(cmd)
		if err != nil {
			return errors.E(err, "cannot setup the database")
		}
	}
	return nil
}

// Acquire attempts to grab the lock with the given key name and wait until it
// succeeds.
func (c *Client) Acquire(name string, opts ...LockOption) (*Lock, error) {
	return c.AcquireContext(context.Background(), name, opts...)
}

// AcquireContext attempts to grab the lock with the given key name, wait until
// it succeeds or the context is done. It returns ErrNotAcquired if the context
// is canceled before the lock is acquired.
func (c *Client) AcquireContext(ctx context.Context, name string, opts ...LockOption) (*Lock, error) {
	l := c.newLock(name, opts)
	for {
		select {
		case <-ctx.Done():
			return nil, ErrNotAcquired
		default:
			err := c.retry(func() error { return c.tryAcquire(ctx, l) })
			if l.failIfLocked && err == ErrNotAcquired {
				c.log.Println("not acquired, exit")
				return l, err
			} else if err == ErrNotAcquired {
				c.log.Println("not acquired, wait:", l.leaseDuration)
				time.Sleep(l.leaseDuration)
				continue
			} else if err != nil {
				c.log.Println("error:", err)
				return nil, err
			}
			return l, nil
		}
	}
}

func (c *Client) tryAcquire(ctx context.Context, l *Lock) error {
	err := c.storeAcquire(ctx, l)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	l.heartbeatCancel = cancel
	go c.heartbeat(ctx, l)
	return nil
}

func (c *Client) storeAcquire(ctx context.Context, l *Lock) error {
	ctx, cancel := context.WithTimeout(ctx, l.leaseDuration)
	defer cancel()
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return typedError(err, "cannot create transaction for lock acquisition")
	}
	rvn, err := c.getNextRVN(ctx, tx)
	if err != nil {
		return typedError(err, "cannot run query to read record version number")
	}
	c.log.Println("storeAcquire in", l.name, rvn, l.data, l.recordVersionNumber)
	defer func() {
		c.log.Println("storeAcquire out", l.name, rvn, l.data, l.recordVersionNumber)
	}()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO `+c.tableName+`
			("name", "record_version_number", "data")
		VALUES
			($1, $2, $3)
		ON CONFLICT ("name") DO UPDATE
		SET
			"record_version_number" = $2,
			"data" = CASE
				WHEN $5 THEN $3
				ELSE `+c.tableName+`."data"
			END
		WHERE
			`+c.tableName+`."record_version_number" IS NULL
			OR `+c.tableName+`."record_version_number" = $4
	`, l.name, rvn, l.data, l.recordVersionNumber, l.replaceData)
	if err != nil {
		return typedError(err, "cannot run query to acquire lock")
	}
	rowLockInfo := tx.QueryRowContext(ctx, `SELECT "record_version_number", "data" FROM `+c.tableName+` WHERE name = $1 FOR UPDATE`, l.name)
	var actualRVN int64
	var data []byte
	if err := rowLockInfo.Scan(&actualRVN, &data); err != nil {
		return typedError(err, "cannot load information for lock acquisition")
	}
	if actualRVN != rvn {
		l.recordVersionNumber = actualRVN
		return ErrNotAcquired
	}
	if err := tx.Commit(); err != nil {
		return typedError(err, "cannot commit lock acquisition")
	}
	l.recordVersionNumber = rvn
	l.data = data
	return nil
}

// Do executes f while holding the lock for the named lock. When the lock loss
// is detected in the heartbeat, it is going to cancel the context passed on to
// f. If it ends normally (err == nil), it releases the lock.
func (c *Client) Do(ctx context.Context, name string, f func(context.Context, *Lock) error, opts ...LockOption) error {
	l := c.newLock(name, opts)
	defer l.Close()
	for {
		select {
		case <-ctx.Done():
			return ErrNotAcquired
		default:
			err := c.retry(func() error { return c.do(ctx, l, f) })
			if l.failIfLocked && err == ErrNotAcquired {
				c.log.Println("not acquired, exit")
				return err
			} else if err == ErrNotAcquired {
				c.log.Println("not acquired, wait:", l.leaseDuration)
				time.Sleep(l.leaseDuration)
				continue
			} else if err != nil {
				c.log.Println("error:", err)
				return err
			}
			return nil
		}
	}
}

func (c *Client) do(ctx context.Context, l *Lock, f func(context.Context, *Lock) error) error {
	err := c.storeAcquire(ctx, l)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.heartbeatCancel = cancel
	go func() {
		defer cancel()
		c.heartbeat(ctx, l)
	}()
	return f(ctx, l)
}

// Release will update the mutex entry to be able to be taken by other clients.
func (c *Client) Release(l *Lock) error {
	return c.ReleaseContext(context.Background(), l)
}

// ReleaseContext will update the mutex entry to be able to be taken by other
// clients.
func (c *Client) ReleaseContext(ctx context.Context, l *Lock) error {
	if l.IsReleased() {
		return ErrLockAlreadyReleased
	}
	return c.retry(func() error { return c.storeRelease(ctx, l) })
}

func (c *Client) storeRelease(ctx context.Context, l *Lock) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, l.leaseDuration)
	defer cancel()
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return typedError(err, "cannot create transaction for lock acquisition")
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE
			`+c.tableName+`
		SET
			"record_version_number" = NULL
		WHERE
			"name" = $1
			AND "record_version_number" = $2
	`, l.name, l.recordVersionNumber)
	if err != nil {
		return typedError(err, "cannot run query to release lock")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return typedError(err, "cannot confirm whether the lock has been released")
	} else if affected == 0 {
		l.isReleased = true
		l.heartbeatCancel()
		return ErrLockAlreadyReleased
	}
	if !l.keepOnRelease {
		_, err := tx.ExecContext(ctx, `
		DELETE FROM
			`+c.tableName+`
		WHERE
			"name" = $1
			AND "record_version_number" IS NULL`, l.name)
		if err != nil {
			return typedError(err, "cannot run query to delete lock")
		}
	}
	if err := tx.Commit(); err != nil {
		return typedError(err, "cannot commit lock release")
	}
	l.isReleased = true
	l.heartbeatCancel()
	return nil
}

func (c *Client) heartbeat(ctx context.Context, l *Lock) {
	if c.heartbeatFrequency <= 0 {
		c.log.Println("heartbeat disabled:", l.name)
		return
	}
	defer c.log.Println("heartbeat stopped:", l.name)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.heartbeatFrequency):
			if err := c.SendHeartbeat(ctx, l); err != nil {
				c.log.Println("heartbeat missed:", l.name, err)
				return
			}
		}
	}
}

// SendHeartbeat refreshes the mutex entry so to avoid other clients from
// grabbing it.
func (c *Client) SendHeartbeat(ctx context.Context, l *Lock) error {
	err := c.retry(func() error { return c.storeHeartbeat(ctx, l) })
	return errors.Wrapf(err, "cannot send heartbeat: %v", l.name)
}

func (c *Client) storeHeartbeat(ctx context.Context, l *Lock) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, l.leaseDuration)
	defer cancel()
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return typedError(err, "cannot create transaction for lock acquisition")
	}
	rvn, err := c.getNextRVN(ctx, tx)
	if err != nil {
		return typedError(err, "cannot run query to read record version number")
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE
			`+c.tableName+`
		SET
			"record_version_number" = $3
		WHERE
			"name" = $1
			AND "record_version_number" = $2
	`, l.name, l.recordVersionNumber, rvn)
	if err != nil {
		return typedError(err, "cannot run query to update the heartbeat")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return typedError(err, "cannot confirm whether the lock has been updated for the heartbeat")
	} else if affected == 0 {
		l.isReleased = true
		return ErrLockAlreadyReleased
	}
	if err := tx.Commit(); err != nil {
		return typedError(err, "cannot commit lock heartbeat")
	}
	l.recordVersionNumber = rvn
	return nil
}

// GetData returns the data field from the given lock in the table
// without holding the lock first.
func (c *Client) GetData(name string) ([]byte, error) {
	return c.GetDataContext(context.Background(), name)
}

// GetDataContext returns the data field from the given lock in the table
// without holding the lock first.
func (c *Client) GetDataContext(ctx context.Context, name string) ([]byte, error) {
	var data []byte
	err := c.retry(func() error {
		var err error
		data, err = c.getLock(ctx, name)
		return err
	})
	if errors.Is(errors.NotExist, err) {
		c.log.Println("missing lock entry:", err)
	}
	return data, err
}

func (c *Client) getLock(ctx context.Context, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.leaseDuration)
	defer cancel()
	row := c.db.QueryRowContext(ctx, `
		SELECT
			"data"
		FROM
			`+c.tableName+`
		WHERE
			"name" = $1
		FOR UPDATE
	`, name)
	var data []byte
	err := row.Scan(&data)
	if err == sql.ErrNoRows {
		return data, ErrLockNotFound
	}
	return data, typedError(err, "cannot load the data of this lock")
}

func (c *Client) getNextRVN(ctx context.Context, tx *sql.Tx) (int64, error) {
	rowRVN := tx.QueryRowContext(ctx, `SELECT nextval('`+c.tableName+`_rvn')`)
	var rvn int64
	err := rowRVN.Scan(&rvn)
	return rvn, err
}

func (c *Client) retry(f func() error) error {
	for {
		err := f()
		if errors.Is(errors.FailedPrecondition, err) {
			c.log.Println("bad transaction, retrying:", err)
			continue
		}
		return err
	}
}

// ClientOption reconfigures the lock client
type ClientOption func(*Client)

// WithLogger injects a logger into the client, so its internals can be
// recorded.
func WithLogger(l Logger) ClientOption {
	return func(c *Client) { c.log = l }
}

// WithLeaseDuration defines how long should the lease be held.
func WithLeaseDuration(d time.Duration) ClientOption {
	return func(c *Client) { c.leaseDuration = d }
}

// WithHeartbeatFrequency defines the frequency of the heartbeats. Heartbeats
// should have no more than half of the duration of the lease.
func WithHeartbeatFrequency(d time.Duration) ClientOption {
	return func(c *Client) { c.heartbeatFrequency = d }
}

// WithCustomTable reconfigures the lock client to use an alternate lock table
// name.
func WithCustomTable(tableName string) ClientOption {
	return func(c *Client) { c.tableName = tableName }
}

func typedError(err error, v ...interface{}) error {
	const serializationErrorCode = "40001"
	if err == nil {
		return nil
	} else if err == sql.ErrNoRows {
		args := append([]interface{}{errors.NotExist, err}, v...)
		return errors.E(args...)
	} else if _, ok := err.(*net.OpError); ok {
		args := append([]interface{}{errors.Unavailable, err}, v...)
		return errors.E(args...)
	} else if e, ok := err.(*pq.Error); ok {
		if e.Code == serializationErrorCode {
			args := append([]interface{}{errors.FailedPrecondition, err}, v...)
			return errors.E(args...)
		}
	}
	return err
}