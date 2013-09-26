package sql

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strconv"
)

func NewPool(sourceStrings []string) (pool *Pool, err error) {
	queue := make(chan *sql.DB, len(sourceStrings))
	var conn *sql.DB
	for _, sourceStr := range sourceStrings {
		conn, err = sql.Open("mysql", sourceStr)
		if err != nil {
			return
		}
		err = conn.Ping()
		if err != nil {
			return
		}

		queue <- conn
	}
	pool = &Pool{queue}
	return
}

type Pool struct {
	queue chan *sql.DB
}

// Query with fixed args
func (p *Pool) Query(query string, args ...interface{}) (rows *sql.Rows, err error) {
	conn := <-p.queue
	defer func() { p.queue <- conn }()

	FixArgs(args)
	rows, err = conn.Query(query, args...)
	if err != nil {
		err = errors.New("sql.Query Error: " + err.Error() + ". Query: " + query + " Args: " + fmt.Sprint(args))
	}
	return
}

// Execute with fixed args
func (p *Pool) Exec(query string, args ...interface{}) (res sql.Result, err error) {
	conn := <-p.queue
	defer func() { p.queue <- conn }()

	FixArgs(args)
	res, err = conn.Exec(query, args...)
	if err != nil {
		err = errors.New("sql.Exec Error: " + err.Error() + ". Query: " + query + " Args: " + fmt.Sprint(args))
	}
	return
}

/*
Fix args by converting them to values of their underlying kind.
This avoids problems in database/sql with e.g custom string types.
Without FixArgs, the following code:

	type Foo string
	...
	pool.Query("SELECT * WHERE Foo=?", Foo("bar"))

would give you the error:

	sql: converting Exec argument #1's type: unsupported type Foo, a string
*/
func FixArgs(args []interface{}) {
	for i, arg := range args {
		vArg := reflect.ValueOf(arg)
		if vArg.Kind() == reflect.String {
			args[i] = vArg.String()
		}
	}
}

func (p *Pool) SelectInt(query string, args ...interface{}) (num int, found bool, err error) {
	found, err = p.queryOne(query, args, &num)
	return
}

func (p *Pool) SelectString(query string, args ...interface{}) (str string, found bool, err error) {
	found, err = p.queryOne(query, args, &str)
	return
}

func (p *Pool) SelectUInt(query string, args ...interface{}) (num uint, found bool, err error) {
	found, err = p.queryOne(query, args, &num)
	return
}

func (p *Pool) queryOne(query string, args []interface{}, out interface{}) (found bool, err error) {
	rows, err := p.Query(query, args...)
	if err != nil {
		return
	}

	if rows.Next() {
		found = true
		err = rows.Scan(out)
		if err != nil {
			return
		}
	}

	if rows.Next() {
		err = errors.New("Query returned too many columns")
		return
	}

	return
}

// type TransactFunc func() error

// func (p *Pool) Transact(f TransactFunc) (err error) {
// 	conn := <-p.queue
// 	defer func() { p.queue <- conn }()

// 	_, err = conn.Exec("START TRANSACTION")
// 	if err != nil {
// 		return
// 	}

// 	err = f(conn)
// 	if err != nil {
// 		_, rollbackError := conn.Exec("ROLLBACK")
// 		if rollbackError != nil {
// 			panic("Could not rollback transaction. TransactionFunc error: " + err.Error() + ". Rollback error:" + rollbackError.Error())
// 		}

// 	} else {
// 		_, err = conn.Exec("COMMIT")
// 	}

// 	return
// }

func (p *Pool) UpdateOne(query string, args ...interface{}) error {
	res, err := p.Exec(query, args...)
	if err != nil {
		return err
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New(fmt.Sprintf("UpdateOne affected %d rows. Query: %q Args: %q", affected, query, args))
	}

	return nil
}

func (p *Pool) InsertIgnoreId(query string, args ...interface{}) (err error) {
	_, err = p.Insert(query, args...)
	return
}

func (p *Pool) Insert(query string, args ...interface{}) (id int64, err error) {
	res, err := p.Exec(query, args...)
	if err != nil {
		return
	}
	id, err = res.LastInsertId()
	return
}

func (p *Pool) Select(output interface{}, sql string, args ...interface{}) error {
	// Check types
	var outputPtr = reflect.ValueOf(output)
	if outputPtr.Kind() != reflect.Ptr {
		return errors.New("fun/sql.Select: expects a pointer to a slice of items")
	}
	var outputReflection = reflect.Indirect(outputPtr)
	if outputReflection.Kind() != reflect.Slice {
		return errors.New("fun/sql.Select: expects items to be a slice")
	}
	if outputReflection.Len() != 0 {
		return errors.New("fun/sql.Select: expects items to be empty")
	}
	outputItemType := outputReflection.Type().Elem().Elem()
	if outputItemType.Kind() != reflect.Struct {
		return errors.New("fun/sql.Select: expects items to be a slice of structs")
	}

	// Query DB
	var rows, err = p.Query(sql, args...)
	if err != nil {
		return err
	}

	// Reflect onto structs
	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	structType := outputReflection.Type().Elem()
	for rows.Next() {
		structPtrVal, err := structFromRow(structType, columns, rows)
		if err != nil {
			return err
		}

		outputReflection.Set(reflect.Append(outputReflection, structPtrVal))
	}
	return nil
}

const selectOneTypeError = "fun/sql.SelectOne: expects a **struct, e.g var person *Person; c.SelectOne(&person, sql)"

func (p *Pool) SelectOne(output interface{}, query string, args ...interface{}) error {
	// Check types
	var outputReflectionPtr = reflect.ValueOf(output)
	if !outputReflectionPtr.IsValid() {
		panic(selectOneTypeError)
	}
	if outputReflectionPtr.Kind() != reflect.Ptr {
		panic(selectOneTypeError)
	}
	var outputReflection = outputReflectionPtr.Elem()
	if outputReflection.Kind() != reflect.Ptr {
		panic(selectOneTypeError)
	}

	// Query DB
	var rows, err = p.Query(query, args...)
	if err != nil {
		return err
	}

	// Reflect onto struct
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	if !rows.Next() {
		return errors.New("fun/sql.SelectOne: got none. Query: " + query + " Args: " + fmt.Sprint(args))
	}

	structType := outputReflection.Type()
	structPtrVal, err := structFromRow(structType, columns, rows)
	if err != nil {
		return err
	}
	outputReflection.Set(structPtrVal)

	if rows.Next() {
		return errors.New("fun/sql.SelectOne: got multiple rows. Query: " + query + " Args: " + fmt.Sprint(args))
	}

	return nil
}

func structFromRow(structType reflect.Type, columns []string, rows *sql.Rows) (structPtrVal reflect.Value, err error) {
	structPtrVal = reflect.New(structType.Elem())

	outputItemStructVal := structPtrVal.Elem()

	vals := make([]interface{}, len(columns))
	for i, _ := range columns {
		vals[i] = &sql.RawBytes{}
		// vals[i] = &[]byte{}
	}
	err = rows.Scan(vals...)
	if err != nil {
		return
	}

	var uintVal uint64
	var intVal int64
	for i, column := range columns {
		bytes := []byte(*vals[i].(*sql.RawBytes))
		// bytes := []byte(*vals[i].(*[]byte))
		if bytes == nil {
			continue // Leave struct field empty
		}
		var outputItemField = outputItemStructVal.FieldByName(column)
		switch outputItemField.Kind() {
		case reflect.String:
			outputItemField.SetString(string(bytes))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			uintVal, err = strconv.ParseUint(string(bytes), 10, 64)
			if err != nil {
				return
			}
			outputItemField.SetUint(reflect.ValueOf(uintVal).Uint())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			intVal, err = strconv.ParseInt(string(bytes), 10, 64)
			if err != nil {
				return
			}
			outputItemField.SetInt(reflect.ValueOf(intVal).Int())
		default:
			err = errors.New("fun/sql: Bad row value for column: " + column)
			return
		}
	}

	return
}
