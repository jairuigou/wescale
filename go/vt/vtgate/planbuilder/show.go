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

package planbuilder

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	vschemapb "vitess.io/vitess/go/vt/proto/vschema"

	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"

	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/vtgate/vindexes"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/key"
	querypb "vitess.io/vitess/go/vt/proto/query"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
)

const (
	utf8    = "utf8"
	utf8mb4 = "utf8mb4"
	both    = "both"
	charset = "charset"
)

func buildShowPlan(sql string, stmt *sqlparser.Show, _ *sqlparser.ReservedVars, vschema plancontext.VSchema) (*planResult, error) {
	// Several SHOW statements related to Vitess cannot be routed to vttablet for execution,
	// need to be processed by vtgate itself.
	if vschema.Destination() != nil {
		var prim engine.Primitive
		var err error
		switch show := stmt.Internal.(type) {
		case *sqlparser.ShowBasic:
			prim, err = buildShowVitessPlan(show, vschema)
			if err != nil {
				return nil, err
			}
			if prim == nil {
				return buildByPassDDLPlan(sql, vschema)
			}
		case *sqlparser.ShowDMLJob:
			prim, err = buildShowDMLJobPlan(show, vschema)
			if err != nil {
				return nil, err
			}
		default:
			return buildByPassDDLPlan(sql, vschema)
		}
		if err != nil {
			return nil, err
		}
		return newPlanResult(prim), nil
	}

	var prim engine.Primitive
	var err error
	switch show := stmt.Internal.(type) {
	case *sqlparser.ShowBasic:
		prim, err = buildShowBasicPlan(show, vschema)
	case *sqlparser.ShowCreate:
		prim, err = buildShowCreatePlan(show, vschema)
	case *sqlparser.ShowOther:
		prim, err = buildShowOtherPlan(sql, vschema)
	default:
		return nil, vterrors.VT13001(fmt.Sprintf("undefined SHOW type: %T", stmt.Internal))
	}
	if err != nil {
		return nil, err
	}

	return newPlanResult(prim), nil
}

func buildShowOtherPlan(sql string, vschema plancontext.VSchema) (engine.Primitive, error) {
	ks, err := vschema.AnyKeyspace()
	if err != nil {
		return nil, err
	}
	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: key.DestinationAnyShard{},
		Query:             sql,
		SingleShardOnly:   true,
	}, nil
}

func buildShowBasicPlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	switch show.Command {
	case sqlparser.Charset:
		return buildCharsetPlan(show)
	case sqlparser.Collation, sqlparser.Function, sqlparser.Privilege, sqlparser.Procedure:
		return buildSendAnywherePlan(show, vschema)
	case sqlparser.VariableGlobal, sqlparser.VariableSession:
		return buildVariablePlan(show, vschema)
	case sqlparser.Column, sqlparser.Index:
		return buildShowTblPlan(show, vschema)
	case sqlparser.Database, sqlparser.Keyspace:
		return buildDBPlan(show, vschema)
	case sqlparser.OpenTable, sqlparser.TableStatus, sqlparser.Table, sqlparser.Trigger:
		return buildPlanWithDB(show, vschema)
	case sqlparser.StatusGlobal, sqlparser.StatusSession:
		return buildSendAnywherePlan(show, vschema)
	case sqlparser.VitessMigrations, sqlparser.SchemaMigration:
		return buildShowVMigrationsPlan(show, vschema)
	case sqlparser.GtidExecGlobal:
		return buildShowGtidPlan(show, vschema)
	case sqlparser.Warnings:
		return buildWarnings()
	case sqlparser.Plugins:
		return buildPluginsPlan()
	case sqlparser.Engines:
		return buildEnginesPlan()
	case sqlparser.VitessReplicationStatus, sqlparser.VitessShards, sqlparser.VitessTablets, sqlparser.VitessVariables, sqlparser.LastSeenGTID, sqlparser.Workload, sqlparser.TabletsPlans:
		return &engine.ShowExec{
			Command:    show.Command,
			ShowFilter: show.Filter,
		}, nil
	case sqlparser.VitessTarget:
		return buildShowTargetPlan(vschema)
	case sqlparser.VschemaTables:
		return buildVschemaTablesPlan(vschema)
	case sqlparser.VschemaVindexes:
		return buildVschemaVindexesPlan(show, vschema)
	}
	return nil, vterrors.VT13001(fmt.Sprintf("unknown SHOW query type %s", show.Command.ToString()))

}

func buildShowVitessPlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	switch show.Command {
	case sqlparser.Keyspace, sqlparser.Database:
		return buildDBPlan(show, vschema)
	case sqlparser.VitessMigrations, sqlparser.SchemaMigration:
		return buildShowVMigrationsPlan(show, vschema)
	case sqlparser.GtidExecGlobal:
		return buildShowGtidPlan(show, vschema)
	case sqlparser.VitessReplicationStatus, sqlparser.VitessShards, sqlparser.VitessTablets, sqlparser.VitessVariables, sqlparser.Workload, sqlparser.LastSeenGTID, sqlparser.FailPoints, sqlparser.TabletsPlans:
		return &engine.ShowExec{
			Command:    show.Command,
			ShowFilter: show.Filter,
		}, nil
	case sqlparser.VitessTarget:
		return buildShowTargetPlan(vschema)
	case sqlparser.VschemaTables:
		return buildVschemaTablesPlan(vschema)
	case sqlparser.VschemaVindexes:
		return buildVschemaVindexesPlan(show, vschema)
	default:
		return nil, nil
	}
}

func buildShowDMLJobPlan(show *sqlparser.ShowDMLJob, vschema plancontext.VSchema) (engine.Primitive, error) {
	dest, ks, tabletType, err := vschema.TargetDestination("")
	if err != nil {
		return nil, err
	}
	if ks == nil {
		return nil, vterrors.VT09005()
	}

	if tabletType != topodatapb.TabletType_PRIMARY {
		return nil, vterrors.VT09006("SHOW")
	}

	if dest == nil {
		dest = key.DestinationAllShards{}
	}

	// Here we don't do any queries, we do queries in func ShowJob of JobController in controller.go
	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: dest,
		Query:             "",
	}, nil
}

func buildShowTargetPlan(vschema plancontext.VSchema) (engine.Primitive, error) {
	rows := [][]sqltypes.Value{sqltypes.BuildVarCharRow(vschema.TargetString())}
	return engine.NewRowsPrimitive(rows,
		sqltypes.BuildVarCharFields("Target")), nil
}

func buildCharsetPlan(show *sqlparser.ShowBasic) (engine.Primitive, error) {
	fields := sqltypes.BuildVarCharFields("Charset", "Description", "Default collation")
	maxLenField := &querypb.Field{Name: "Maxlen", Type: sqltypes.Int32}
	fields = append(fields, maxLenField)

	charsets := []string{utf8, utf8mb4}
	rows, err := generateCharsetRows(show.Filter, charsets)
	if err != nil {
		return nil, err
	}

	return engine.NewRowsPrimitive(rows, fields), nil
}

func buildSendAnywherePlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	ks, err := vschema.AnyKeyspace()
	if err != nil {
		return nil, err
	}
	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: key.DestinationAnyShard{},
		Query:             sqlparser.String(show),
		IsDML:             false,
		SingleShardOnly:   true,
	}, nil
}

func buildVariablePlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	plan, err := buildSendAnywherePlan(show, vschema)
	if err != nil {
		return nil, err
	}
	plan = engine.NewReplaceVariables(plan)
	return plan, nil
}

func buildShowTblPlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	if !show.DbName.IsEmpty() {
		show.Tbl.Qualifier = sqlparser.NewIdentifierCS(show.DbName.String())
		// Remove Database Name from the query.
		show.DbName = sqlparser.NewIdentifierCS("")
	}

	dest := key.Destination(key.DestinationAnyShard{})
	var ks *vindexes.Keyspace
	var err error

	if !show.Tbl.Qualifier.IsEmpty() && sqlparser.SystemSchema(show.Tbl.Qualifier.String()) {
		ks, err = vschema.AnyKeyspace()
		if err != nil {
			return nil, err
		}
	} else {
		table, _, _, _, destination, err := vschema.FindTableOrVindex(show.Tbl)
		if err != nil {
			return nil, err
		}
		if table == nil {
			return nil, vterrors.VT05004(show.Tbl.Name.String())
		}
		// Update the table.
		show.Tbl.Qualifier = sqlparser.NewIdentifierCS("")
		show.Tbl.Name = table.Name

		if destination != nil {
			dest = destination
		}
		ks = table.Keyspace
	}

	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: dest,
		Query:             sqlparser.String(show),
		IsDML:             false,
		SingleShardOnly:   true,
	}, nil
}

func buildDBPlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	ks, err := vschema.AllKeyspace()
	if err != nil {
		return nil, err
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].Name < ks[j].Name })

	var filter *regexp.Regexp

	if show.Filter != nil {
		filter = sqlparser.LikeToRegexp(show.Filter.Like)
	}

	if filter == nil {
		filter = regexp.MustCompile(".*")
	}

	// rows := make([][]sqltypes.Value, 0, len(ks)+4)
	var rows [][]sqltypes.Value

	for _, v := range ks {
		if filter.MatchString(v.Name) {
			rows = append(rows, sqltypes.BuildVarCharRow(v.Name))
		}
	}
	return engine.NewRowsPrimitive(rows, sqltypes.BuildVarCharFields("Database")), nil
}

// buildShowVMigrationsPlan serves `SHOW VITESS_MIGRATIONS ...` queries. It invokes queries on mysql.schema_migrations on all PRIMARY tablets on keyspace's shards.
func buildShowVMigrationsPlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	dest, ks, tabletType, err := vschema.TargetDestination(show.DbName.String())
	if err != nil {
		return nil, err
	}
	if ks == nil {
		return nil, vterrors.VT09005()
	}

	if tabletType != topodatapb.TabletType_PRIMARY {
		return nil, vterrors.VT09006("SHOW")
	}

	if dest == nil {
		dest = key.DestinationAllShards{}
	}

	sql := "SELECT * FROM mysql.schema_migrations"

	if show.Filter != nil {
		if show.Filter.Filter != nil {
			sql += fmt.Sprintf(" where %s", sqlparser.String(show.Filter.Filter))
		} else if show.Filter.Like != "" {
			lit := sqlparser.String(sqlparser.NewStrLiteral(show.Filter.Like))
			sql += fmt.Sprintf(" where migration_uuid LIKE %s OR migration_context LIKE %s OR migration_status LIKE %s", lit, lit, lit)
		}
	}
	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: dest,
		Query:             sql,
	}, nil
}

func buildPlanWithDB(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	dbName := show.DbName
	dbDestination := show.DbName.String()
	if sqlparser.SystemSchema(dbDestination) {
		ks, err := vschema.AnyKeyspace()
		if err != nil {
			return nil, err
		}
		dbDestination = ks.Name
	} else {
		// Remove Database Name from the query.
		show.DbName = sqlparser.NewIdentifierCS("")
	}
	destination, keyspace, _, err := vschema.TargetDestination(dbDestination)
	if err != nil {
		return nil, err
	}
	if destination == nil {
		destination = key.DestinationAnyShard{}
	}

	if dbName.IsEmpty() {
		dbName = sqlparser.NewIdentifierCS(keyspace.Name)
	}

	query := sqlparser.String(show)
	var plan engine.Primitive
	plan = &engine.Send{
		Keyspace:          keyspace,
		TargetDestination: destination,
		Query:             query,
		IsDML:             false,
		SingleShardOnly:   true,
	}
	if show.Command == sqlparser.Table {
		plan, err = engine.NewRenameField([]string{"Tables_in_" + dbName.String()}, []int{0}, plan)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil

}

func generateCharsetRows(showFilter *sqlparser.ShowFilter, colNames []string) ([][]sqltypes.Value, error) {
	if showFilter == nil {
		return buildCharsetRows(both), nil
	}

	var filteredColName string
	var err error

	if showFilter.Like != "" {
		filteredColName, err = checkLikeOpt(showFilter.Like, colNames)
		if err != nil {
			return nil, err
		}

	} else {
		cmpExp, ok := showFilter.Filter.(*sqlparser.ComparisonExpr)
		if !ok {
			return nil, vterrors.VT12001("expect a 'LIKE' or '=' expression")
		}

		left, ok := cmpExp.Left.(*sqlparser.ColName)
		if !ok {
			return nil, vterrors.VT12001("expect left side to be 'charset'")
		}
		leftOk := left.Name.EqualString(charset)

		if leftOk {
			literal, ok := cmpExp.Right.(*sqlparser.Literal)
			if !ok {
				return nil, vterrors.VT12001("we expect the right side to be a string")
			}
			rightString := literal.Val

			switch cmpExp.Operator {
			case sqlparser.EqualOp:
				for _, colName := range colNames {
					if rightString == colName {
						filteredColName = colName
					}
				}
			case sqlparser.LikeOp:
				filteredColName, err = checkLikeOpt(rightString, colNames)
				if err != nil {
					return nil, err
				}
			}
		}

	}

	return buildCharsetRows(filteredColName), nil
}

func buildCharsetRows(colName string) [][]sqltypes.Value {
	row0 := sqltypes.BuildVarCharRow(
		"utf8",
		"UTF-8 Unicode",
		"utf8_general_ci")
	row0 = append(row0, sqltypes.NewInt32(3))
	row1 := sqltypes.BuildVarCharRow(
		"utf8mb4",
		"UTF-8 Unicode",
		"utf8mb4_general_ci")
	row1 = append(row1, sqltypes.NewInt32(4))

	switch colName {
	case utf8:
		return [][]sqltypes.Value{row0}
	case utf8mb4:
		return [][]sqltypes.Value{row1}
	case both:
		return [][]sqltypes.Value{row0, row1}
	}

	return [][]sqltypes.Value{}
}

func checkLikeOpt(likeOpt string, colNames []string) (string, error) {
	likeRegexp := strings.ReplaceAll(likeOpt, "%", ".*")
	for _, v := range colNames {
		match, err := regexp.MatchString(likeRegexp, v)
		if err != nil {
			return "", err
		}
		if match {
			return v, nil
		}
	}

	return "", nil
}

func buildShowCreatePlan(show *sqlparser.ShowCreate, vschema plancontext.VSchema) (engine.Primitive, error) {
	switch show.Command {
	case sqlparser.CreateDb:
		return buildCreateDbPlan(show, vschema)
	case sqlparser.CreateE, sqlparser.CreateF, sqlparser.CreateProc, sqlparser.CreateTr, sqlparser.CreateV:
		return buildCreatePlan(show, vschema)
	case sqlparser.CreateTbl:
		return buildCreateTblPlan(show, vschema)
	}
	return nil, vterrors.VT13001("unknown SHOW query type %s", show.Command.ToString())
}

func buildCreateDbPlan(show *sqlparser.ShowCreate, vschema plancontext.VSchema) (engine.Primitive, error) {
	dbName := show.Op.Name.String()
	if sqlparser.SystemSchema(dbName) {
		ks, err := vschema.AnyKeyspace()
		if err != nil {
			return nil, err
		}
		dbName = ks.Name
	}

	dest, ks, _, err := vschema.TargetDestination(dbName)
	if err != nil {
		return nil, err
	}

	if dest == nil {
		dest = key.DestinationAnyShard{}
	}

	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: dest,
		Query:             sqlparser.String(show),
		IsDML:             false,
		SingleShardOnly:   true,
	}, nil
}

func buildCreateTblPlan(show *sqlparser.ShowCreate, vschema plancontext.VSchema) (engine.Primitive, error) {
	dest := key.Destination(key.DestinationAnyShard{})
	var ks *vindexes.Keyspace
	var err error

	if !show.Op.Qualifier.IsEmpty() && sqlparser.SystemSchema(show.Op.Qualifier.String()) {
		ks, err = vschema.AnyKeyspace()
		if err != nil {
			return nil, err
		}
	} else {
		tbl, _, _, _, destKs, err := vschema.FindTableOrVindex(show.Op)
		if err != nil {
			return nil, err
		}
		if tbl == nil {
			return nil, vterrors.VT05004(sqlparser.String(show.Op))
		}
		ks = tbl.Keyspace
		if destKs != nil {
			dest = destKs
		}
		show.Op.Qualifier = sqlparser.NewIdentifierCS("")
		show.Op.Name = tbl.Name
	}

	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: dest,
		Query:             sqlparser.String(show),
		IsDML:             false,
		SingleShardOnly:   true,
	}, nil

}

func buildCreatePlan(show *sqlparser.ShowCreate, vschema plancontext.VSchema) (engine.Primitive, error) {
	dbName := ""
	if !show.Op.Qualifier.IsEmpty() {
		dbName = show.Op.Qualifier.String()
	}

	if sqlparser.SystemSchema(dbName) {
		ks, err := vschema.AnyKeyspace()
		if err != nil {
			return nil, err
		}
		dbName = ks.Name
	} else {
		show.Op.Qualifier = sqlparser.NewIdentifierCS("")
	}

	dest, ks, _, err := vschema.TargetDestination(dbName)
	if err != nil {
		return nil, err
	}
	if dest == nil {
		dest = key.DestinationAnyShard{}
	}

	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: dest,
		Query:             sqlparser.String(show),
		IsDML:             false,
		SingleShardOnly:   true,
	}, nil

}

func buildShowGtidPlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	dbName := ""
	if !show.DbName.IsEmpty() {
		dbName = show.DbName.String()
	}
	dest, ks, _, err := vschema.TargetDestination(dbName)
	if err != nil {
		return nil, err
	}
	if dest == nil {
		dest = key.DestinationAllShards{}
	}

	return &engine.Send{
		Keyspace:          ks,
		TargetDestination: dest,
		Query:             fmt.Sprintf(`select '%s' as db_name, @@global.gtid_executed as gtid_executed`, ks.Name),
	}, nil
}

func buildWarnings() (engine.Primitive, error) {

	f := func(sa engine.SessionActions) (*sqltypes.Result, error) {
		fields := []*querypb.Field{
			{Name: "Level", Type: sqltypes.VarChar},
			{Name: "Code", Type: sqltypes.Uint16},
			{Name: "Message", Type: sqltypes.VarChar},
		}

		warns := sa.GetWarnings()
		rows := make([][]sqltypes.Value, 0, len(warns))

		for _, warn := range warns {
			rows = append(rows, []sqltypes.Value{
				sqltypes.NewVarChar("Warning"),
				sqltypes.NewUint32(warn.Code),
				sqltypes.NewVarChar(warn.Message),
			})
		}
		return &sqltypes.Result{
			Fields: fields,
			Rows:   rows,
		}, nil
	}

	return engine.NewSessionPrimitive("SHOW WARNINGS", f), nil
}

func buildPluginsPlan() (engine.Primitive, error) {
	var rows [][]sqltypes.Value
	rows = append(rows, sqltypes.BuildVarCharRow(
		"InnoDB",
		"ACTIVE",
		"STORAGE ENGINE",
		"NULL",
		"GPL"))

	return engine.NewRowsPrimitive(rows,
		sqltypes.BuildVarCharFields("Name", "Status", "Type", "Library", "License")), nil
}

func buildEnginesPlan() (engine.Primitive, error) {
	var rows [][]sqltypes.Value
	rows = append(rows, sqltypes.BuildVarCharRow(
		"InnoDB",
		"DEFAULT",
		"Supports transactions, row-level locking, and foreign keys",
		"YES",
		"YES",
		"YES"))

	return engine.NewRowsPrimitive(rows,
		sqltypes.BuildVarCharFields("Engine", "Support", "Comment", "Transactions", "XA", "Savepoints")), nil
}

func buildVschemaTablesPlan(vschema plancontext.VSchema) (engine.Primitive, error) {
	vs := vschema.GetVSchema()
	ks, err := vschema.DefaultKeyspace()
	if err != nil {
		return nil, err
	}
	schemaKs, ok := vs.Keyspaces[ks.Name]
	if !ok {
		return nil, vterrors.VT05003(ks.Name)
	}

	var tables []string
	for name := range schemaKs.Tables {
		tables = append(tables, name)
	}
	sort.Strings(tables)

	rows := make([][]sqltypes.Value, len(tables))
	for i, v := range tables {
		rows[i] = sqltypes.BuildVarCharRow(v)
	}

	return engine.NewRowsPrimitive(rows, sqltypes.BuildVarCharFields("Tables")), nil
}

func buildVschemaVindexesPlan(show *sqlparser.ShowBasic, vschema plancontext.VSchema) (engine.Primitive, error) {
	vs := vschema.GetSrvVschema()
	rows := make([][]sqltypes.Value, 0, 16)

	if !show.Tbl.IsEmpty() {
		_, ks, _, err := vschema.TargetDestination(show.Tbl.Qualifier.String())
		if err != nil {
			return nil, err
		}
		var schemaKs *vschemapb.Keyspace
		var tbl *vschemapb.Table
		if !ks.Sharded {
			tbl = &vschemapb.Table{}
		} else {
			schemaKs = vs.Keyspaces[ks.Name]
			tableName := show.Tbl.Name.String()
			schemaTbl, ok := schemaKs.Tables[tableName]
			if !ok {
				return nil, vterrors.VT05005(tableName, ks.Name)
			}
			tbl = schemaTbl
		}

		for _, colVindex := range tbl.ColumnVindexes {
			vindex, ok := schemaKs.Vindexes[colVindex.GetName()]
			columns := colVindex.GetColumns()
			if len(columns) == 0 {
				columns = []string{colVindex.GetColumn()}
			}
			if ok {
				params := make([]string, 0, 4)
				for k, v := range vindex.GetParams() {
					params = append(params, fmt.Sprintf("%s=%s", k, v))
				}
				sort.Strings(params)
				rows = append(rows, sqltypes.BuildVarCharRow(strings.Join(columns, ", "), colVindex.GetName(), vindex.GetType(), strings.Join(params, "; "), vindex.GetOwner()))
			} else {
				rows = append(rows, sqltypes.BuildVarCharRow(strings.Join(columns, ", "), colVindex.GetName(), "", "", ""))
			}
		}

		return engine.NewRowsPrimitive(rows,
			sqltypes.BuildVarCharFields("Columns", "Name", "Type", "Params", "Owner"),
		), nil
	}

	// For the query interface to be stable we need to sort
	// for each of the map iterations
	ksNames := make([]string, 0, len(vs.Keyspaces))
	for name := range vs.Keyspaces {
		ksNames = append(ksNames, name)
	}
	sort.Strings(ksNames)
	for _, ksName := range ksNames {
		ks := vs.Keyspaces[ksName]

		vindexNames := make([]string, 0, len(ks.Vindexes))
		for name := range ks.Vindexes {
			vindexNames = append(vindexNames, name)
		}
		sort.Strings(vindexNames)
		for _, vindexName := range vindexNames {
			vindex := ks.Vindexes[vindexName]

			params := make([]string, 0, 4)
			for k, v := range vindex.GetParams() {
				params = append(params, fmt.Sprintf("%s=%s", k, v))
			}
			sort.Strings(params)
			rows = append(rows, sqltypes.BuildVarCharRow(ksName, vindexName, vindex.GetType(), strings.Join(params, "; "), vindex.GetOwner()))
		}
	}
	return engine.NewRowsPrimitive(rows,
		sqltypes.BuildVarCharFields("Keyspace", "Name", "Type", "Params", "Owner"),
	), nil

}
