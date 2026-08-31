package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// scanPlan is the precomputed mapping from a query's columns onto a T. It is
// built once per query, not once per row.
type scanPlan[T any] struct {
	// fieldForCol[i] is the struct field index chain for column i, or nil when
	// the query returned a column this T does not care about.
	fieldForCol [][]int
	scalar      bool
}

// planFor works out how to turn one row of cols into a T.
//
// A struct T maps columns onto fields by `db:"..."` tag, else by snake_casing
// the field name. Anything else (string, int64, a sql.Scanner) is treated as a
// single-column scalar.
func planFor[T any](cols []string) (scanPlan[T], error) {
	var zero T
	rt := reflect.TypeOf(&zero).Elem()

	if !isStructRow(rt) {
		if len(cols) != 1 {
			return scanPlan[T]{}, fmt.Errorf("scanning into %s wants exactly one column, query returned %d: %v", rt, len(cols), cols)
		}
		return scanPlan[T]{scalar: true}, nil
	}

	byName := map[string][]int{}
	collectFields(rt, nil, byName)

	plan := scanPlan[T]{fieldForCol: make([][]int, len(cols))}
	used := map[string]bool{}
	for i, c := range cols {
		key := strings.ToLower(c)
		if idx, ok := byName[key]; ok {
			plan.fieldForCol[i] = idx
			used[key] = true
		}
		// An unmapped column is fine and common: SELECT * against a table that
		// grew a column should not break every caller.
	}

	// A struct field with no matching column is the opposite case: the caller
	// and the SQL have drifted, and silently leaving it zero would hide a bug.
	var missing []string
	for name := range byName {
		if !used[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return scanPlan[T]{}, fmt.Errorf("scanning into %s: no column for field(s) %s (query returned %s)",
			rt, strings.Join(sortedUnique(missing), ", "), strings.Join(cols, ", "))
	}
	return plan, nil
}

// scan reads the current row into dst.
func (p scanPlan[T]) scan(rows *sql.Rows, dst *T) error {
	rv := reflect.ValueOf(dst).Elem()

	if p.scalar {
		if s, ok := any(dst).(sql.Scanner); ok {
			return rows.Scan(s)
		}
		var raw any
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		return assign(rv, raw)
	}

	targets := make([]any, len(p.fieldForCol))
	raws := make([]any, len(p.fieldForCol))
	for i, idx := range p.fieldForCol {
		if idx == nil {
			targets[i] = new(any) // column we are deliberately ignoring
			continue
		}
		field := rv.FieldByIndex(idx)
		// sql.Scanner implementations (sql.NullString and friends) know how to
		// handle their own NULLs; let them.
		if field.CanAddr() {
			if s, ok := field.Addr().Interface().(sql.Scanner); ok {
				targets[i] = s
				continue
			}
		}
		targets[i] = &raws[i]
	}

	if err := rows.Scan(targets...); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	for i, idx := range p.fieldForCol {
		if idx == nil || targets[i] != &raws[i] {
			continue
		}
		if err := assign(rv.FieldByIndex(idx), raws[i]); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}
	return nil
}

// isStructRow reports whether T should be scanned field by field. time.Time and
// anything implementing sql.Scanner are structs but are single values.
func isStructRow(rt reflect.Type) bool {
	if rt.Kind() != reflect.Struct {
		return false
	}
	if rt == reflect.TypeFor[time.Time]() {
		return false
	}
	return !reflect.PointerTo(rt).Implements(reflect.TypeFor[sql.Scanner]())
}

// collectFields walks a struct, including embedded ones, recording the column
// name each exported field answers to.
func collectFields(rt reflect.Type, prefix []int, out map[string][]int) {
	for i := range rt.NumField() {
		f := rt.Field(i)
		idx := append(append([]int{}, prefix...), i)

		tag := f.Tag.Get("db")
		if tag == "-" {
			continue
		}
		// An embedded struct is traversed even when its own type name is
		// unexported. reflect still reaches and sets the exported fields it
		// promotes, and `struct { noteRow; Snippet string }` is exactly how the
		// index composes a search-hit row on top of a note row.
		if f.Anonymous && tag == "" && isStructRow(f.Type) {
			collectFields(f.Type, idx, out)
			continue
		}
		if !f.IsExported() {
			continue
		}
		name := tag
		if name == "" {
			name = snakeCase(f.Name)
		}
		out[strings.ToLower(name)] = idx
	}
}

// snakeCase turns a Go field name into the column name we expect:
// "TailscaleUserID" becomes "tailscale_user_id", "SHA256" becomes "sha256".
func snakeCase(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' && i > 0 {
			prevLower := runes[i-1] >= 'a' && runes[i-1] <= 'z' || runes[i-1] >= '0' && runes[i-1] <= '9'
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// assign converts a driver value into a Go field. The driver hands back a small
// set of types (nil, int64, float64, bool, []byte, string, time.Time) and this
// is the one place that knows how to widen them.
//
// A NULL leaves the field at its zero value rather than erroring, which is what
// makes "SELECT a_nullable_column" into a plain string field work.
func assign(dst reflect.Value, src any) error {
	if src == nil {
		dst.Set(reflect.Zero(dst.Type()))
		return nil
	}
	if dst.Kind() == reflect.Pointer {
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		return assign(dst.Elem(), src)
	}
	// Exact type match, including time.Time.
	if sv := reflect.ValueOf(src); sv.Type().AssignableTo(dst.Type()) {
		dst.Set(sv)
		return nil
	}

	switch dst.Kind() {
	case reflect.String:
		switch v := src.(type) {
		case string:
			dst.SetString(v)
		case []byte:
			dst.SetString(string(v))
		case int64:
			dst.SetString(strconv.FormatInt(v, 10))
		case float64:
			dst.SetString(strconv.FormatFloat(v, 'g', -1, 64))
		case time.Time:
			dst.SetString(v.Format(time.RFC3339Nano))
		default:
			return fmt.Errorf("cannot convert %T to string", src)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := toInt(src)
		if err != nil {
			return err
		}
		if dst.OverflowInt(n) {
			return fmt.Errorf("%d overflows %s", n, dst.Type())
		}
		dst.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := toInt(src)
		if err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("cannot store %d in %s", n, dst.Type())
		}
		dst.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		switch v := src.(type) {
		case float64:
			dst.SetFloat(v)
		case int64:
			dst.SetFloat(float64(v))
		case []byte:
			f, err := strconv.ParseFloat(string(v), 64)
			if err != nil {
				return err
			}
			dst.SetFloat(f)
		case string:
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}
			dst.SetFloat(f)
		default:
			return fmt.Errorf("cannot convert %T to %s", src, dst.Type())
		}
	case reflect.Bool:
		switch v := src.(type) {
		case bool:
			dst.SetBool(v)
		case int64:
			dst.SetBool(v != 0)
		case []byte:
			dst.SetBool(len(v) > 0 && v[0] != '0')
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return err
			}
			dst.SetBool(b)
		default:
			return fmt.Errorf("cannot convert %T to bool", src)
		}
	case reflect.Slice:
		if dst.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("cannot convert %T to %s", src, dst.Type())
		}
		switch v := src.(type) {
		case []byte:
			dst.SetBytes(append([]byte(nil), v...))
		case string:
			dst.SetBytes([]byte(v))
		default:
			return fmt.Errorf("cannot convert %T to []byte", src)
		}
	default:
		return fmt.Errorf("cannot convert %T to %s", src, dst.Type())
	}
	return nil
}

func toInt(src any) (int64, error) {
	switch v := src.(type) {
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case []byte:
		return strconv.ParseInt(string(v), 10, 64)
	case string:
		return strconv.ParseInt(v, 10, 64)
	case time.Time:
		return v.Unix(), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to an integer", src)
	}
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
