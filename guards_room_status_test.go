package main

import (
	"go/ast"
	"go/token"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// ---------------------------------------------------------------------------
// room_bookings.status and room_requests.status each have one writer of an
// existing row's status: changeBookings, and moveRoomRequest with
// moveRoomRequestsWhere for a set. Their WHEREs are the moves' from-sets in
// internal/policy (room_lifecycle.go). placeBooking inserts a booking's first
// status and openRoomRequest a request's (docs/agents/lifecycles.md).
//
// Parse-only, every production package. A room row value is a name a function
// binds to models.RoomBooking or models.RoomRequest (or a pointer or slice of
// one): a parameter, a var of that type, an assignment from a literal, from
// another row value or from a declared function whose first result is one, or
// a range over a slice of them. A room chain is a gorm call chain with
// Model(<row value>) or Table("room_bookings" or "room_requests"), or a name
// assigned one. On a room chain the guard reports Update or UpdateColumn of
// "status" or of a column it cannot read, and Updates or UpdateColumns of a
// map holding a "status" key (a literal, or a local map given one), of a
// parameter's map, of a map with computed keys, or of a row struct. Anywhere
// it reports a Create, CreateInBatches, FirstOrCreate or Save of a row value
// (an insert writes the status, the column's default included), and raw SQL
// that inserts into either table, or updates one and sets status or builds
// its column list at run time.
//
// It does not see a row reached through a struct field (bookWholeSeries reads
// bookings into a struct that embeds one), an interface or a result of a
// function the scanned tree does not declare; a package that imports models
// under another name; an upsert's DoUpdates; gorm calls in a function literal
// at package level (there it reads only raw SQL); or SQL assembled without the
// "UPDATE <table> SET" or "INSERT INTO <table>" literal, or a status set after
// a subquery in the SET list (it reads the list up to the first FROM or
// WHERE). It does not cover
// request_id, which also moves a booking between waiting and lost
// (BookingCannotConfirm writes it through changeBookings; the foreign key's ON
// DELETE SET NULL is the database's). It does not judge which status a
// creator inserts or which move a caller passes: the policy table and the
// writers' refusals decide those.
// ---------------------------------------------------------------------------

// roomStatusWriterCensus is every write of either status column the guard
// sees.
var roomStatusWriterCensus = []string{
	"changeBookings: raw UPDATE room_bookings with a column list built at run time",
	"moveRoomRequest: Updates of a map given \"status\"",
	"moveRoomRequestsWhere: raw UPDATE of room_requests.status",
	"openRoomRequest: Create of a room request",
	"placeBooking: raw INSERT INTO room_bookings",
}

// roomStatusTables maps each table to its model and the words a report uses.
var roomStatusTables = map[string]struct{ model, row string }{
	"room_bookings": {"RoomBooking", "room booking"},
	"room_requests": {"RoomRequest", "room request"},
}

var (
	roomUpdateRe     = regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?(?:public\.)?"?(room_bookings|room_requests)"?(?:\s+(?:AS\s+)?\w+)?\s+SET\b(.*?)(?:\bWHERE\b|\bFROM\b|\bRETURNING\b|;|$)`)
	roomInsertRe     = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(?:public\.)?"?(room_bookings|room_requests)"?(?:\s|\(|$)`)
	roomSetsStatusRe = regexp.MustCompile(`(?i)(?:^|[\s,."])status"?\s*=`)
)

type roomStatusWrite struct{ at, fn, shape string }

// roomRowType is the table of a models.RoomBooking or models.RoomRequest, a
// pointer to one, or a slice of either; "" for any other type.
func roomRowType(e ast.Expr) string {
	switch v := guardkit.Unparen(e).(type) {
	case *ast.StarExpr:
		return roomRowType(v.X)
	case *ast.ArrayType:
		return roomRowType(v.Elt)
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok && x.Name == "models" {
			for table, t := range roomStatusTables {
				if v.Sel.Name == t.model {
					return table
				}
			}
		}
	}
	return ""
}

// roomRowValue is the table of e when e is a row literal or a row name, or
// the address or an element of one.
func roomRowValue(e ast.Expr, names map[string]string) string {
	switch v := guardkit.Unparen(e).(type) {
	case *ast.CompositeLit:
		return roomRowType(v.Type)
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return roomRowValue(v.X, names)
		}
	case *ast.StarExpr:
		return roomRowValue(v.X, names)
	case *ast.IndexExpr:
		return roomRowValue(v.X, names)
	case *ast.Ident:
		return names[v.Name]
	}
	return ""
}

// roomRowFuncs maps every declared function whose first result is a row to
// its table.
func roomRowFuncs(p *guardkit.Parsed) map[string]string {
	out := map[string]string{}
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
				if table := roomRowType(fd.Type.Results.List[0].Type); table != "" {
					out[fd.Name.Name] = table
				}
			}
		}
	}
	return out
}

// roomNames maps every name fd binds to a row value, and every name it
// assigns a room chain, to the table.
func roomNames(fd *ast.FuncDecl, funcs map[string]string) (rows, chains map[string]string) {
	rows, chains = map[string]string{}, map[string]string{}
	params := func(ft *ast.FuncType) {
		for _, fl := range []*ast.FieldList{ft.Params, ft.Results} {
			if fl == nil {
				continue
			}
			for _, f := range fl.List {
				if table := roomRowType(f.Type); table != "" {
					for _, n := range f.Names {
						rows[n.Name] = table
					}
				}
			}
		}
	}
	params(fd.Type)
	if fd.Recv != nil {
		params(&ast.FuncType{Params: fd.Recv})
	}
	// Twice, so a name bound from one bound later in the source still counts.
	for range 2 {
		ast.Inspect(fd, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncLit:
				params(v.Type)
			case *ast.ValueSpec:
				for i, name := range v.Names {
					if v.Type != nil && roomRowType(v.Type) != "" {
						rows[name.Name] = roomRowType(v.Type)
					} else if i < len(v.Values) && roomRowValue(v.Values[i], rows) != "" {
						rows[name.Name] = roomRowValue(v.Values[i], rows)
					}
				}
			case *ast.AssignStmt:
				for i, l := range v.Lhs {
					id, ok := l.(*ast.Ident)
					if !ok {
						continue
					}
					var rhs ast.Expr
					if len(v.Rhs) == len(v.Lhs) {
						rhs = v.Rhs[i]
					} else if len(v.Rhs) == 1 && i == 0 {
						rhs = v.Rhs[0]
					}
					if rhs == nil {
						continue
					}
					if table := roomRowValue(rhs, rows); table != "" {
						rows[id.Name] = table
					} else if call, ok := guardkit.Unparen(rhs).(*ast.CallExpr); ok && funcs[guardkit.Callee(call)] != "" {
						rows[id.Name] = funcs[guardkit.Callee(call)]
					} else if table := roomChain(rhs, rows, chains); table != "" {
						chains[id.Name] = table
					}
				}
			case *ast.RangeStmt:
				if table := roomRowValue(v.X, rows); table != "" {
					if id, ok := v.Value.(*ast.Ident); ok {
						rows[id.Name] = table
					}
				}
			}
			return true
		})
	}
	return rows, chains
}

// roomChain is the table a gorm call chain names, or "".
func roomChain(x ast.Expr, rows, chains map[string]string) string {
	for {
		switch v := guardkit.Unparen(x).(type) {
		case *ast.Ident:
			return chains[v.Name]
		case *ast.CallExpr:
			sel, ok := guardkit.Unparen(v.Fun).(*ast.SelectorExpr)
			if !ok {
				return ""
			}
			switch sel.Sel.Name {
			case "Model":
				if len(v.Args) == 1 {
					if table := roomRowValue(v.Args[0], rows); table != "" {
						return table
					}
				}
			case "Table":
				if len(v.Args) > 0 {
					if s, ok := stringLit(v.Args[0]); ok && len(strings.Fields(s)) > 0 {
						if _, room := roomStatusTables[strings.Fields(s)[0]]; room {
							return strings.Fields(s)[0]
						}
					}
				}
			}
			x = sel.X
		default:
			return ""
		}
	}
}

func roomMapHasStatus(e ast.Expr) bool {
	lit, ok := guardkit.Unparen(e).(*ast.CompositeLit)
	if !ok {
		return false
	}
	if _, ok := lit.Type.(*ast.MapType); !ok {
		return false
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if s, ok := stringLit(kv.Key); ok && s == "status" {
				return true
			}
		}
	}
	return false
}

// roomLocalMaps is what fd does to each local map: given a "status" key (in
// its literal or by assignment), or given a computed key; and every parameter
// name of fd and its function literals.
func roomLocalMaps(fd *ast.FuncDecl) (status, computed, params map[string]bool) {
	status, computed, params = map[string]bool{}, map[string]bool{}, map[string]bool{}
	addParams := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				params[n.Name] = true
			}
		}
	}
	addParams(fd.Type.Params)
	ast.Inspect(fd, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			addParams(v.Type.Params)
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if i < len(v.Values) && roomMapHasStatus(v.Values[i]) {
					status[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, l := range v.Lhs {
				switch lv := l.(type) {
				case *ast.Ident:
					if i < len(v.Rhs) && roomMapHasStatus(v.Rhs[i]) {
						status[lv.Name] = true
					}
				case *ast.IndexExpr:
					x, ok := guardkit.Unparen(lv.X).(*ast.Ident)
					if !ok {
						continue
					}
					if s, ok := stringLit(lv.Index); !ok {
						computed[x.Name] = true
					} else if s == "status" {
						status[x.Name] = true
					}
				}
			}
		}
		return true
	})
	return status, computed, params
}

func roomMapShape(e ast.Expr, status, computed, params map[string]bool) string {
	if roomMapHasStatus(e) {
		return `a map with "status"`
	}
	id, ok := guardkit.Unparen(e).(*ast.Ident)
	if !ok {
		return ""
	}
	switch {
	case status[id.Name]:
		return `a map given "status"`
	case params[id.Name]:
		return "a parameter's map"
	case computed[id.Name]:
		return "a map with computed keys"
	}
	return ""
}

// roomStatusWrites finds every write of either status column in p.
func roomStatusWrites(p *guardkit.Parsed) []roomStatusWrite {
	var out []roomStatusWrite
	funcs := roomRowFuncs(p)
	add := func(n ast.Node, fn, shape string) {
		out = append(out, roomStatusWrite{p.Fset.Position(n.Pos()).String(), fn, shape})
	}
	rawSQL := func(root ast.Node, fn string) {
		ast.Inspect(root, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok {
				return true
			}
			s, _ := stringLit(lit)
			for _, m := range roomInsertRe.FindAllStringSubmatch(s, -1) {
				add(n, fn, "raw INSERT INTO "+strings.ToLower(m[1]))
			}
			for _, m := range roomUpdateRe.FindAllStringSubmatch(s, -1) {
				table := strings.ToLower(m[1])
				if roomSetsStatusRe.MatchString(m[2]) {
					add(n, fn, "raw UPDATE of "+table+".status")
				} else if strings.TrimSpace(m[2]) == "" {
					add(n, fn, "raw UPDATE "+table+" with a column list built at run time")
				}
			}
			return true
		})
	}
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && (gd.Tok == token.VAR || gd.Tok == token.CONST) {
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					rawSQL(vs, vs.Names[0].Name)
				}
				continue
			}
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			rawSQL(fd.Body, name)
			rows, chains := roomNames(fd, funcs)
			status, computed, params := roomLocalMaps(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := guardkit.Unparen(call.Fun).(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				method, arg := sel.Sel.Name, call.Args[0]
				switch method {
				case "Create", "CreateInBatches", "FirstOrCreate", "Save":
					if table := roomRowValue(arg, rows); table != "" {
						verb := method
						if method != "Save" {
							verb = "Create"
						}
						add(call, name, verb+" of a "+roomStatusTables[table].row)
					}
					return true
				}
				table := roomChain(sel.X, rows, chains)
				if table == "" {
					return true
				}
				switch method {
				case "Update", "UpdateColumn":
					if s, lit := stringLit(arg); lit && s == "status" {
						add(call, name, method+`("status") of a `+roomStatusTables[table].row)
					} else if !lit {
						add(call, name, method+" of a computed column of a "+roomStatusTables[table].row)
					}
				case "Updates", "UpdateColumns":
					if roomRowValue(arg, rows) != "" {
						add(call, name, method+" of a "+roomStatusTables[table].row+" struct")
					} else if shape := roomMapShape(arg, status, computed, params); shape != "" {
						add(call, name, method+" of "+shape)
					}
				}
				return true
			})
		}
	}
	return out
}

func roomStatusCensus(writes []roomStatusWrite) []string {
	got := []string{}
	for _, w := range writes {
		got = append(got, w.fn+": "+w.shape)
	}
	sort.Strings(got)
	return got
}

func TestRoomStatusHasOneWriter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := roomStatusWrites(parsed)
	if got := roomStatusCensus(writes); !reflect.DeepEqual(got, roomStatusWriterCensus) {
		var where []string
		for _, w := range writes {
			where = append(where, w.at+" "+w.fn+": "+w.shape)
		}
		t.Errorf("the writes of room_bookings.status and room_requests.status are\n%s\nwant exactly %v; change a booking's status with changeBookings and a policy.RoomBookingMove, a request's with moveRoomRequest and a policy.RoomRequestMove", strings.Join(where, "\n"), roomStatusWriterCensus)
	}
}

func TestRoomStatusGuardCatchesBypasses(t *testing.T) {
	const decls = `
func loadRequest(tx *gorm.DB, id uint) (*models.RoomRequest, error) { return nil, nil }
`
	for _, c := range []struct{ name, src, want string }{
		{"a confirmation as it was written", `package main
func confirm(tx *gorm.DB, b models.RoomBooking) error {
	return tx.Exec(` + "`UPDATE room_bookings SET status = ?, updated_at = now() WHERE id = ?`" + `, models.BookingConfirmed, b.ID).Error
}`, "confirm: raw UPDATE of room_bookings.status"},
		{"a move with status last, as it was written", `package main
func move(tx *gorm.DB, id uint) error {
	return tx.Exec(` + "`UPDATE room_bookings SET starts_at = ?, padded_range = tstzrange(?, ?), status = ?, updated_at = now() WHERE id = ?`" + `).Error
}`, "move: raw UPDATE of room_bookings.status"},
		{"a withdrawal as it was written", `package main
func withdraw(tx *gorm.DB, eventID uint) error {
	return tx.Exec(` + "`UPDATE room_requests SET status = ?, updated_at = now()\n\t\tWHERE event_id = ? AND series AND status = ? RETURNING id`" + `).Error
}`, "withdraw: raw UPDATE of room_requests.status"},
		{"a decision as it was written", `package main
func decide(tx *gorm.DB, req *models.RoomRequest) error {
	return tx.Model(req).Updates(map[string]any{"status": models.RoomRequestConfirmed, "decided_at": time.Now()}).Error
}`, `decide: Updates of a map with "status"`},
		{"a query held in a variable", `package main
func decide(tx *gorm.DB, id uint) error {
	q := tx.Model(&models.RoomRequest{}).Where("id = ?", id)
	return q.Updates(map[string]any{"status": "rejected"}).Error
}`, `decide: Updates of a map with "status"`},
		{"a map given status later", `package main
func decide(tx *gorm.DB, id uint) error {
	fields := map[string]any{"decided_at": time.Now()}
	fields["status"] = models.RoomRequestRejected
	return tx.Model(&models.RoomRequest{}).Where("id = ?", id).Updates(fields).Error
}`, `decide: Updates of a map given "status"`},
		{"a bare Update on a loaded request", `package main
func decide(tx *gorm.DB, id uint) error {
	r, _ := loadRequest(tx, id)
	return tx.Model(r).Update("status", models.RoomRequestWithdrawn).Error
}` + decls, `decide: Update("status") of a room request`},
		{"a range over bookings", `package main
func reject(tx *gorm.DB) error {
	var bookings []models.RoomBooking
	for _, b := range bookings {
		tx.Model(&b).UpdateColumn("status", models.BookingRejected)
	}
	return nil
}`, `reject: UpdateColumn("status") of a room booking`},
		{"the table by name", `package main
func reject(tx *gorm.DB, id uint) error {
	return tx.Table("room_bookings").Where("request_id = ?", id).Update("status", "rejected").Error
}`, `reject: Update("status") of a room booking`},
		{"a column named at run time", `package main
func set(tx *gorm.DB, b *models.RoomBooking, col string) error {
	return tx.Model(b).Update(col, "confirmed").Error
}`, "set: Update of a computed column of a room booking"},
		{"a helper that writes its caller's columns", `package main
func writeRequest(tx *gorm.DB, id uint, cols map[string]any) error {
	return tx.Model(&models.RoomRequest{}).Where("id = ?", id).Updates(cols).Error
}`, "writeRequest: Updates of a parameter's map"},
		{"a saved booking", `package main
func confirm(tx *gorm.DB, b *models.RoomBooking) error {
	b.Status = models.BookingConfirmed
	return tx.Save(b).Error
}`, "confirm: Save of a room booking"},
		{"a booking struct as the update", `package main
func confirm(tx *gorm.DB, id uint) error {
	return tx.Model(&models.RoomBooking{}).Where("id = ?", id).Updates(models.RoomBooking{Status: "confirmed"}).Error
}`, "confirm: Updates of a room booking struct"},
		{"a new booking through gorm", `package main
func book(tx *gorm.DB) error {
	b := models.RoomBooking{RoomID: 1}
	return tx.Create(&b).Error
}`, "book: Create of a room booking"},
		{"a raw request insert", `package main
func ask(tx *gorm.DB) error {
	return tx.Exec("INSERT INTO room_requests (event_id, room_id) VALUES (1, 1)").Error
}`, "ask: raw INSERT INTO room_requests"},
		{"raw SQL in a constant, with an alias", `package main
const rejectSQL = ` + "`UPDATE room_bookings AS b SET \"status\" = 'rejected' WHERE b.request_id = ?`" + `
`, "rejectSQL: raw UPDATE of room_bookings.status"},
		{"raw SQL with its columns built at run time", `package main
func set(tx *gorm.DB, cols string) error {
	return tx.Exec("UPDATE room_requests SET " + cols + " WHERE id = 1").Error
}`, "set: raw UPDATE room_requests with a column list built at run time"},
		{"another table's status, and other room columns", `package main
func other(tx *gorm.DB, req *models.RoomRequest, ev *models.Event, id uint) error {
	tx.Model(ev).Updates(map[string]any{"status": "active", "default_room_id": nil})
	tx.Model(&models.Membership{}).Where("id = ?", id).Update("status", "left")
	tx.Exec("UPDATE event_occurrences SET status = 'cancelled' WHERE id = ?", id)
	tx.Exec("UPDATE room_bookings SET request_id = NULL WHERE status = 'requested'")
	tx.Exec("UPDATE room_requests SET reject_reason = '' WHERE status = 'pending'")
	tx.Model(req).Update("reject_reason", "")
	tx.Delete(req)
	return tx.Where("occurrence_id = ?", id).Delete(&models.RoomBooking{}).Error
}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := guardkit.Sources{"x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(roomStatusCensus(roomStatusWrites(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
