package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var sqlite = syscall.NewLazyDLL("winsqlite3.dll")

//go:uintptrescapes
func sqlcall(name string, args ...uintptr) uintptr {
	v, _, _ := sqlite.NewProc(name).Call(args...)
	return v
}

type localSessionTitle struct {
	Text  string
	Named bool
}

func sqliteTitles(path string, ids map[string]bool) map[string]localSessionTitle {
	result := map[string]localSessionTitle{}
	if _, e := os.Stat(path); e != nil {
		return result
	}
	if e := sqlite.Load(); e != nil {
		return result
	}
	name, _ := syscall.BytePtrFromString(path)
	var db uintptr
	rc := sqlcall("sqlite3_open_v2", uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&db)), 1, 0)
	if db != 0 {
		defer sqlcall("sqlite3_close", db)
	}
	if rc != 0 {
		return result
	}
	sqlcall("sqlite3_busy_timeout", db, 50)
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	if len(keys) == 0 {
		return result
	}
	query := "SELECT id, COALESCE(NULLIF(name,''),title), COALESCE(name,'') FROM threads WHERE id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")"
	q, _ := syscall.BytePtrFromString(query)
	var stmt uintptr
	rc = sqlcall("sqlite3_prepare_v2", db, uintptr(unsafe.Pointer(q)), ^uintptr(0), uintptr(unsafe.Pointer(&stmt)), 0)
	if stmt != 0 {
		defer sqlcall("sqlite3_finalize", stmt)
	}
	if rc != 0 {
		return result
	}
	for i, id := range keys {
		b := []byte(id)
		if len(b) > 0 {
			sqlcall("sqlite3_bind_text", stmt, uintptr(i+1), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), ^uintptr(0))
		}
	}
	column := func(i uintptr) string {
		p := sqlcall("sqlite3_column_text", stmt, i)
		n := sqlcall("sqlite3_column_bytes", stmt, i)
		if p == 0 || n == 0 || n > 1<<20 {
			return ""
		}
		b := make([]byte, int(n))
		kernel32.NewProc("RtlMoveMemory").Call(uintptr(unsafe.Pointer(&b[0])), p, n)
		return string(b)
	}
	for sqlcall("sqlite3_step", stmt) == 100 {
		id, title := column(0), column(1)
		if strings.TrimSpace(title) != "" {
			result[id] = localSessionTitle{title, strings.TrimSpace(column(2)) != ""}
		}
	}
	return result
}
func readSessionTitleInfo(home string, ids map[string]bool) map[string]localSessionTitle {
	titles := sqliteTitles(filepath.Join(home, "state_5.sqlite"), ids)
	databaseIDs := map[string]bool{}
	for id := range titles {
		databaseIDs[id] = true
	}
	// Bounded fallback for clients that only maintain the JSONL title index.
	f, e := os.Open(filepath.Join(home, "session_index.jsonl"))
	if e != nil {
		return titles
	}
	defer f.Close()
	stat, e := f.Stat()
	if e != nil {
		return titles
	}
	start := max(int64(0), stat.Size()-(4<<20))
	scanner := bufio.NewScanner(io.NewSectionReader(f, start, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var row struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil && ids[row.ID] && row.Name != "" && !databaseIDs[row.ID] {
			titles[row.ID] = localSessionTitle{row.Name, true}
		}
	}
	return titles
}

func readSessionTitles(home string, ids map[string]bool) map[string]string {
	result := map[string]string{}
	for id, title := range readSessionTitleInfo(home, ids) {
		result[id] = title.Text
	}
	return result
}
