/*
Copyright ApeCloud, Inc.
Licensed under the Apache v2(found in the LICENSE file in the root directory).
*/

/*
Copyright 2020 The Vitess Authors.

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

package tabletserver

import (
	"context"
	"time"

	"vitess.io/vitess/go/pools"
	"vitess.io/vitess/go/sync2"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/connpool"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tx"

	querypb "vitess.io/vitess/go/vt/proto/query"
)

const (
	scpClosed = int64(iota)
	scpOpen
	scpKillingNonTx
	scpKillingAll
)

// StatefulConnectionPool keeps track of currently and future active connections
// it's used whenever the session has some state that requires a dedicated connection
type StatefulConnectionPool struct {
	env tabletenv.Env

	state sync2.AtomicInt64

	// conns is the 'regular' pool. By default, connections
	// are pulled from here for starting transactions.
	conns *connpool.Pool

	// foundRowsPool is the alternate pool that creates
	// connections with CLIENT_FOUND_ROWS flag set. A separate
	// pool is needed because this option can only be set at
	// connection time.
	foundRowsPool *connpool.Pool

	// connsWithoutDB and foundRowsWithoutDBPool is pool that connections
	// without database.
	connsWithoutDB         *connpool.Pool
	foundRowsWithoutDBPool *connpool.Pool
	active                 *pools.Numbered
	lastID                 sync2.AtomicInt64
}

// NewStatefulConnPool creates an ActivePool
func NewStatefulConnPool(env tabletenv.Env) *StatefulConnectionPool {
	config := env.Config()

	return &StatefulConnectionPool{
		env:           env,
		conns:         connpool.NewPool(env, "TransactionPool", config.TxPool),
		foundRowsPool: connpool.NewPool(env, "FoundRowsPool", config.TxPool),
		connsWithoutDB: connpool.NewPool(env, "TransactionWithoutDBPool", tabletenv.ConnPoolConfig{
			Size:               2,
			TimeoutSeconds:     config.TxPool.TimeoutSeconds,
			IdleTimeoutSeconds: config.TxPool.IdleTimeoutSeconds,
			MaxLifetimeSeconds: config.TxPool.MaxLifetimeSeconds,
			MaxWaiters:         config.TxPool.MaxWaiters,
		}),
		foundRowsWithoutDBPool: connpool.NewPool(env, "FoundRowsWithoutDBPool", tabletenv.ConnPoolConfig{
			Size:               2,
			TimeoutSeconds:     config.TxPool.TimeoutSeconds,
			IdleTimeoutSeconds: config.TxPool.IdleTimeoutSeconds,
			MaxLifetimeSeconds: config.TxPool.MaxLifetimeSeconds,
			MaxWaiters:         config.TxPool.MaxWaiters,
		}),
		active: pools.NewNumbered(),
		lastID: sync2.NewAtomicInt64(time.Now().UnixNano()),
	}
}

// Open makes the TxPool operational. This also starts the transaction killer
// that will kill long-running transactions.
func (sf *StatefulConnectionPool) Open(appParams, dbaParams, appDebugParams dbconfigs.Connector) {
	log.Infof("Starting transaction id: %d", sf.lastID)
	sf.conns.Open(appParams, dbaParams, appDebugParams)
	foundRowsParam, _ := appParams.MysqlParams()
	foundRowsParam.EnableClientFoundRows()
	appFoundRowsParams := dbconfigs.New(foundRowsParam)
	sf.foundRowsPool.Open(appFoundRowsParams, dbaParams, appDebugParams)

	newAppParam, _ := appParams.MysqlParams()
	newAppParam.SetDBName("")
	appWithoutDBParams := dbconfigs.New(newAppParam)
	newDbaParam, _ := dbaParams.MysqlParams()
	newDbaParam.SetDBName("")
	dbaWithoutDBParams := dbconfigs.New(newDbaParam)
	newAppDebugParam, _ := appDebugParams.MysqlParams()
	newAppDebugParam.SetDBName("")
	debugWithoutDBParams := dbconfigs.New(newAppDebugParam)
	sf.connsWithoutDB.Open(appWithoutDBParams, dbaWithoutDBParams, debugWithoutDBParams)

	newFoundRowsParam, _ := appFoundRowsParams.MysqlParams()
	newFoundRowsParam.SetDBName("")
	appFoundRowsWithoutDBParam := dbconfigs.New(newFoundRowsParam)
	sf.foundRowsWithoutDBPool.Open(appFoundRowsWithoutDBParam, dbaWithoutDBParams, debugWithoutDBParams)

	sf.state.Set(scpOpen)
}

// Close closes the TxPool. A closed pool can be reopened.
func (sf *StatefulConnectionPool) Close() {
	for _, v := range sf.active.GetByFilter("for closing", func(_ any) bool { return true }) {
		conn := v.(*StatefulConnection)
		thing := "connection"
		if conn.IsInTransaction() {
			thing = "transaction"
		}
		log.Warningf("killing %s for shutdown: %s", thing, conn.String(sf.env.Config().SanitizeLogMessages))
		sf.env.Stats().InternalErrors.Add("StrayTransactions", 1)
		conn.Close()
		conn.Releasef("pool closed")
	}
	sf.conns.Close()
	sf.foundRowsPool.Close()
	sf.connsWithoutDB.Close()
	sf.foundRowsWithoutDBPool.Close()
	sf.state.Set(scpClosed)
}

// ShutdownNonTx enters the state where all non-transactional connections are killed.
// InUse connections will be killed as they are returned.
func (sf *StatefulConnectionPool) ShutdownNonTx() {
	sf.state.Set(scpKillingNonTx)
	conns := mapToTxConn(sf.active.GetByFilter("kill non-tx", func(sc any) bool {
		return !sc.(*StatefulConnection).IsInTransaction()
	}))
	for _, sc := range conns {
		sc.Releasef("kill non-tx")
	}
}

// ShutdownAll enters the state where all connections are to be killed.
// It returns all connections that are not in use. They must be rolled back
// by the caller (TxPool). InUse connections will be killed as they are returned.
func (sf *StatefulConnectionPool) ShutdownAll() []*StatefulConnection {
	sf.state.Set(scpKillingAll)
	return mapToTxConn(sf.active.GetByFilter("kill non-tx", func(sc any) bool {
		return true
	}))
}

// AdjustLastID adjusts the last transaction id to be at least
// as large as the input value. This will ensure that there are
// no dtid collisions with future transactions.
func (sf *StatefulConnectionPool) AdjustLastID(id int64) {
	if current := sf.lastID.Get(); current < id {
		log.Infof("Adjusting transaction id to: %d", id)
		sf.lastID.Set(id)
	}
}

// GetElapsedTimeout returns sessions older than the timeout stored on the
// connection. Does not return any connections that are in use.
// TODO(sougou): deprecate.
func (sf *StatefulConnectionPool) GetElapsedTimeout(purpose string) []*StatefulConnection {
	return mapToTxConn(sf.active.GetByFilter(purpose, func(val any) bool {
		sc := val.(*StatefulConnection)
		return sc.ElapsedTimeout()
	}))
}

func mapToTxConn(vals []any) []*StatefulConnection {
	result := make([]*StatefulConnection, len(vals))
	for i, el := range vals {
		result[i] = el.(*StatefulConnection)
	}
	return result
}

// WaitForEmpty returns as soon as the pool becomes empty
func (sf *StatefulConnectionPool) WaitForEmpty() {
	sf.active.WaitForEmpty()
}

// GetAndLock locks the connection for use. It accepts a purpose as a string.
// If it cannot be found, it returns a "not found" error. If in use,
// it returns a "in use: purpose" error.
func (sf *StatefulConnectionPool) GetAndLock(id int64, reason string) (*StatefulConnection, error) {
	conn, err := sf.active.Get(id, reason)
	if err != nil {
		return nil, err
	}
	return conn.(*StatefulConnection), nil
}

// NewConn creates a new StatefulConnection. It will be created from either the normal pool or
// the found_rows pool, depending on the options provided
func (sf *StatefulConnectionPool) NewConn(ctx context.Context, options *querypb.ExecuteOptions, setting *pools.Setting) (*StatefulConnection, error) {
	var conn *connpool.DBConn
	var err error

	if setting != nil && setting.GetWithoutDBName() {
		if options.GetClientFoundRows() {
			conn, err = sf.foundRowsWithoutDBPool.Get(ctx, setting)
		} else {
			conn, err = sf.connsWithoutDB.Get(ctx, setting)
		}
	} else {
		if options.GetClientFoundRows() {
			conn, err = sf.foundRowsPool.Get(ctx, setting)
		} else {
			conn, err = sf.conns.Get(ctx, setting)
		}
	}
	if err != nil {
		return nil, err
	}

	connID := sf.lastID.Add(1)
	sfConn := &StatefulConnection{
		dbConn:         conn,
		ConnID:         connID,
		pool:           sf,
		env:            sf.env,
		enforceTimeout: options.GetWorkload() != querypb.ExecuteOptions_DBA,
	}
	// This will set both the timeout and initialize the expiryTime.
	sfConn.SetTimeout(sf.env.Config().TxTimeoutForWorkload(options.GetWorkload()))

	err = sf.active.Register(sfConn.ConnID, sfConn)
	if err != nil {
		sfConn.Release(tx.ConnInitFail)
		return nil, err
	}

	return sf.GetAndLock(sfConn.ConnID, "new connection")
}

// ForAllTxProperties executes a function an every connection that has a not-nil TxProperties
func (sf *StatefulConnectionPool) ForAllTxProperties(f func(*tx.Properties)) {
	for _, connection := range mapToTxConn(sf.active.GetAll()) {
		props := connection.txProps
		if props != nil {
			f(props)
		}
	}
}

// Unregister forgets the specified connection.  If the connection is not present, it's ignored.
func (sf *StatefulConnectionPool) unregister(id tx.ConnID, reason string) {
	sf.active.Unregister(id, reason)
}

// markAsNotInUse marks the connection as not in use at the moment
func (sf *StatefulConnectionPool) markAsNotInUse(sc *StatefulConnection, updateTime bool) {
	switch sf.state.Get() {
	case scpKillingNonTx:
		if !sc.IsInTransaction() {
			sc.Releasef("kill non-tx")
			return
		}
	case scpKillingAll:
		if sc.IsInTransaction() {
			sc.Close()
		}
		sc.Releasef("kill all")
		return
	}
	if updateTime {
		sc.resetExpiryTime()
	}
	sf.active.Put(sc.ConnID)
}

// Capacity returns the pool capacity.
func (sf *StatefulConnectionPool) Capacity() int {
	return int(sf.conns.Capacity())
}

// renewConn unregister and registers with new id.
func (sf *StatefulConnectionPool) renewConn(sc *StatefulConnection) error {
	sf.active.Unregister(sc.ConnID, "renew existing connection")
	sc.ConnID = sf.lastID.Add(1)
	sc.resetExpiryTime()
	return sf.active.Register(sc.ConnID, sc)
}

// InUse returns the sum of in-use connections
func (sf *StatefulConnectionPool) InUse() int64 {
	return sf.conns.InUse() + sf.connsWithoutDB.InUse() + sf.foundRowsPool.InUse() + sf.foundRowsWithoutDBPool.InUse()
}

func (sf *StatefulConnectionPool) CloseIdleConnections(max int) int {
	return sf.conns.CloseIdleConnections(max) +
		sf.connsWithoutDB.CloseIdleConnections(max) +
		sf.foundRowsPool.CloseIdleConnections(max) +
		sf.foundRowsWithoutDBPool.CloseIdleConnections(max)
}
