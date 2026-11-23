package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type columnInfo struct {
	Name       string
	DataType   string
	IsNullable bool
	IsPrimary  bool
	HasDefault bool
	IsAutoInc  bool
}

type tableInfo struct {
	Name    string
	Columns []columnInfo
	ByName  map[string]*columnInfo
	PK      *columnInfo
}

type DBExplorer struct {
	db      *sql.DB
	tables  map[string]*tableInfo
	ordered []string
}

func NewDBExplorer(db *sql.DB) (http.Handler, error) {
	e := &DBExplorer{
		db:     db,
		tables: make(map[string]*tableInfo),
	}
	if err := e.loadSchema(); err != nil {
		return nil, err
	}
	return http.HandlerFunc(e.route), nil
}

func (e *DBExplorer) loadSchema() error {
	rows, err := e.db.Query("SHOW TABLES")
	if err != nil {
		return err
	}

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, name := range names {
		ti, err := e.loadTable(name)
		if err != nil {
			return err
		}
		e.tables[name] = ti
		e.ordered = append(e.ordered, name)
	}
	return nil
}

func (e *DBExplorer) loadTable(name string) (*tableInfo, error) {
	safe := strings.ReplaceAll(name, "`", "``")
	rows, err := e.db.Query("SHOW FULL COLUMNS FROM `" + safe + "`")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int, len(cols))
	for i, c := range cols {
		idx[strings.ToLower(c)] = i
	}

	ti := &tableInfo{Name: name}

	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}

		getStr := func(key string) string {
			i, ok := idx[key]
			if !ok || vals[i] == nil {
				return ""
			}
			switch v := vals[i].(type) {
			case []byte:
				return string(v)
			case string:
				return v
			default:
				return fmt.Sprint(v)
			}
		}

		var def interface{}
		if i, ok := idx["default"]; ok {
			def = vals[i]
		}

		ti.Columns = append(ti.Columns, columnInfo{
			Name:       getStr("field"),
			DataType:   simplifyType(strings.ToLower(getStr("type"))),
			IsNullable: strings.EqualFold(getStr("null"), "YES"),
			IsPrimary:  strings.EqualFold(getStr("key"), "PRI"),
			HasDefault: def != nil,
			IsAutoInc:  strings.Contains(strings.ToLower(getStr("extra")), "auto_increment"),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ti.ByName = make(map[string]*columnInfo, len(ti.Columns))
	for i := range ti.Columns {
		ti.ByName[ti.Columns[i].Name] = &ti.Columns[i]
		if ti.Columns[i].IsPrimary {
			ti.PK = &ti.Columns[i]
		}
	}
	return ti, nil
}

func simplifyType(t string) string {
	switch {
	case strings.HasPrefix(t, "int"),
		strings.HasPrefix(t, "tinyint"),
		strings.HasPrefix(t, "smallint"),
		strings.HasPrefix(t, "mediumint"),
		strings.HasPrefix(t, "bigint"),
		strings.HasPrefix(t, "bit"),
		strings.HasPrefix(t, "year"):
		return "int"
	case strings.HasPrefix(t, "float"),
		strings.HasPrefix(t, "double"),
		strings.HasPrefix(t, "decimal"),
		strings.HasPrefix(t, "numeric"),
		strings.HasPrefix(t, "real"):
		return "float"
	default:
		return "string"
	}
}

func (e *DBExplorer) route(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	var parts []string
	if path != "" {
		parts = strings.Split(path, "/")
	}

	switch len(parts) {
	case 0:
		e.handleTables(w, r)
	case 1:
		ti, ok := e.tables[parts[0]]
		if !ok {
			writeError(w, http.StatusNotFound, "unknown table")
			return
		}
		switch r.Method {
		case http.MethodGet:
			e.handleList(w, r, ti)
		case http.MethodPut:
			e.handleCreate(w, r, ti)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case 2:
		ti, ok := e.tables[parts[0]]
		if !ok {
			writeError(w, http.StatusNotFound, "unknown table")
			return
		}
		switch r.Method {
		case http.MethodGet:
			e.handleGet(w, r, ti, parts[1])
		case http.MethodPost:
			e.handleUpdate(w, r, ti, parts[1])
		case http.MethodDelete:
			e.handleDelete(w, r, ti, parts[1])
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (e *DBExplorer) handleTables(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"response": map[string]interface{}{"tables": e.ordered},
	})
}

func (e *DBExplorer) handleList(w http.ResponseWriter, r *http.Request, ti *tableInfo) {
	limit, offset := 5, 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}

	query := fmt.Sprintf("SELECT * FROM `%s` LIMIT %d OFFSET %d", ti.Name, limit, offset)
	rows, err := e.db.Query(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	records, err := scanRows(rows, ti)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"response": map[string]interface{}{"records": records},
	})
}

func (e *DBExplorer) handleGet(w http.ResponseWriter, r *http.Request, ti *tableInfo, id string) {
	rec, err := e.fetchRecord(ti, id)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"response": map[string]interface{}{"record": rec},
	})
}

func (e *DBExplorer) fetchRecord(ti *tableInfo, id string) (map[string]interface{}, error) {
	if ti.PK == nil {
		return nil, fmt.Errorf("no primary key")
	}
	rows, err := e.db.Query(
		"SELECT * FROM `"+ti.Name+"` WHERE `"+ti.PK.Name+"` = ? LIMIT 1", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records, err := scanRows(rows, ti)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, sql.ErrNoRows
	}
	return records[0], nil
}

func (e *DBExplorer) handleCreate(w http.ResponseWriter, r *http.Request, ti *tableInfo) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	values := make(map[string]interface{})
	for _, col := range ti.Columns {
		raw, present := body[col.Name]
		if !present {
			continue
		}
		if col.IsPrimary && col.IsAutoInc {
			continue
		}
		v, ok := convertValue(raw, col)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("field %s have invalid type", col.Name))
			return
		}
		values[col.Name] = v
	}

	for _, col := range ti.Columns {
		if _, ok := values[col.Name]; ok {
			continue
		}
		if !col.IsNullable && !col.HasDefault && !col.IsAutoInc {
			values[col.Name] = zeroValue(col.DataType)
		}
	}

	if len(values) == 0 {
		writeError(w, http.StatusBadRequest, "no fields to insert")
		return
	}

	var cols, ph []string
	var args []interface{}
	for _, col := range ti.Columns {
		if v, ok := values[col.Name]; ok {
			cols = append(cols, "`"+col.Name+"`")
			ph = append(ph, "?")
			args = append(args, v)
		}
	}

	res, err := e.db.Exec(
		"INSERT INTO `"+ti.Name+"` ("+strings.Join(cols, ",")+
			") VALUES ("+strings.Join(ph, ",")+")", args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := map[string]interface{}{}
	if ti.PK != nil {
		if ti.PK.IsAutoInc {
			if id, err := res.LastInsertId(); err == nil {
				resp[ti.PK.Name] = id
			}
		} else if v, ok := values[ti.PK.Name]; ok {
			resp[ti.PK.Name] = v
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"response": resp})
}

func (e *DBExplorer) handleUpdate(w http.ResponseWriter, r *http.Request, ti *tableInfo, id string) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if ti.PK != nil {
		if _, ok := body[ti.PK.Name]; ok {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("field %s have invalid type", ti.PK.Name))
			return
		}
	}

	var sets []string
	var args []interface{}
	for _, col := range ti.Columns {
		raw, present := body[col.Name]
		if !present {
			continue
		}
		v, ok := convertValue(raw, col)
		if !ok {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("field %s have invalid type", col.Name))
			return
		}
		sets = append(sets, "`"+col.Name+"` = ?")
		args = append(args, v)
	}

	if len(sets) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"response": map[string]interface{}{"updated": 0},
		})
		return
	}
	if ti.PK == nil {
		writeError(w, http.StatusInternalServerError, "no primary key")
		return
	}

	args = append(args, id)
	res, err := e.db.Exec(
		"UPDATE `"+ti.Name+"` SET "+strings.Join(sets, ", ")+
			" WHERE `"+ti.PK.Name+"` = ?", args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	affected, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"response": map[string]interface{}{"updated": affected},
	})
}

func (e *DBExplorer) handleDelete(w http.ResponseWriter, r *http.Request, ti *tableInfo, id string) {
	if ti.PK == nil {
		writeError(w, http.StatusInternalServerError, "no primary key")
		return
	}
	res, err := e.db.Exec("DELETE FROM `"+ti.Name+"` WHERE `"+ti.PK.Name+"` = ?", id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	affected, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"response": map[string]interface{}{"deleted": affected},
	})
}

func convertValue(raw interface{}, col columnInfo) (interface{}, bool) {
	if raw == nil {
		if col.IsNullable {
			return nil, true
		}
		return nil, false
	}
	switch col.DataType {
	case "int":
		f, ok := raw.(float64)
		if !ok || f != float64(int64(f)) {
			return nil, false
		}
		return int64(f), true
	case "float":
		f, ok := raw.(float64)
		if !ok {
			return nil, false
		}
		return f, true
	case "string":
		s, ok := raw.(string)
		if !ok {
			return nil, false
		}
		return s, true
	}
	return nil, false
}

func zeroValue(dataType string) interface{} {
	switch dataType {
	case "int":
		return int64(0)
	case "float":
		return float64(0)
	default:
		return ""
	}
}

func readBody(r *http.Request) (map[string]interface{}, error) {
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body == nil {
		body = map[string]interface{}{}
	}
	return body, nil
}

func scanRows(rows *sql.Rows, ti *tableInfo) ([]map[string]interface{}, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := make(map[string]interface{}, len(cols))
		for i, name := range cols {
			rec[name] = normalizeDBValue(vals[i], ti.ByName[name])
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func normalizeDBValue(v interface{}, col *columnInfo) interface{} {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []byte:
		s := string(t)
		if col != nil {
			switch col.DataType {
			case "int":
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					return n
				}
			case "float":
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					return f
				}
			}
		}
		return s
	case int64, float64, string:
		return t
	case bool:
		if t {
			return int64(1)
		}
		return int64(0)
	default:
		return fmt.Sprint(t)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{"error": msg})
}
