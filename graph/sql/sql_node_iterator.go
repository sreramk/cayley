// Copyright 2015 The Cayley Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sql

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/barakmich/glog"
	"github.com/google/cayley/graph"
	"github.com/google/cayley/graph/iterator"
	"github.com/google/cayley/quad"
)

var sqlNodeType graph.Type

func init() {
	sqlNodeType = graph.RegisterIterator("sqlnode")
}

type SQLNodeIterator struct {
	uid       uint64
	qs        *QuadStore
	tagger    graph.Tagger
	tableName string
	err       error

	cursor  *sql.Rows
	linkIts []sqlItDir
	size    int64
	tagdirs []tagDir

	result      map[string]string
	resultIndex int
	resultList  [][]string
	resultNext  [][]string
	cols        []string
}

func (n *SQLNodeIterator) sqlClone() sqlIterator {
	return n.Clone().(*SQLNodeIterator)
}

func (n *SQLNodeIterator) Clone() graph.Iterator {
	m := &SQLNodeIterator{
		uid:       iterator.NextUID(),
		qs:        n.qs,
		size:      n.size,
		tableName: n.tableName,
	}
	for _, i := range n.linkIts {
		m.linkIts = append(m.linkIts, sqlItDir{
			dir: i.dir,
			it:  i.it.sqlClone(),
		})
	}
	copy(n.tagdirs, m.tagdirs)
	m.tagger.CopyFrom(n)
	return m
}

func (n *SQLNodeIterator) UID() uint64 {
	return n.uid
}

func (n *SQLNodeIterator) Reset() {
	n.err = nil
	n.Close()
}

func (n *SQLNodeIterator) Err() error {
	return n.err
}

func (n *SQLNodeIterator) Close() error {
	if n.cursor != nil {
		err := n.cursor.Close()
		if err != nil {
			return err
		}
		n.cursor = nil
	}
	return nil
}

func (n *SQLNodeIterator) Tagger() *graph.Tagger {
	return &n.tagger
}

func (n *SQLNodeIterator) Result() graph.Value {
	return n.result["__execd"]
}

func (n *SQLNodeIterator) TagResults(dst map[string]graph.Value) {
	for tag, value := range n.result {
		if tag == "__execd" {
			for _, tag := range n.tagger.Tags() {
				dst[tag] = value
			}
			continue
		}
		dst[tag] = value
	}

	for tag, value := range n.tagger.Fixed() {
		dst[tag] = value
	}
}

func (n *SQLNodeIterator) Type() graph.Type {
	return sqlNodeType
}

func (n *SQLNodeIterator) SubIterators() []graph.Iterator {
	// TODO(barakmich): SQL Subiterators shouldn't count? If it makes sense,
	// there's no reason not to expose them though.
	return nil
}

func (n *SQLNodeIterator) Sorted() bool                     { return false }
func (n *SQLNodeIterator) Optimize() (graph.Iterator, bool) { return n, false }

func (n *SQLNodeIterator) Size() (int64, bool) {
	return n.qs.Size() / int64(len(n.linkIts)+1), true
}

func (n *SQLNodeIterator) Describe() graph.Description {
	size, _ := n.Size()
	return graph.Description{
		UID:  n.UID(),
		Name: fmt.Sprintf("SQL_NODE_QUERY: %#v", n),
		Type: n.Type(),
		Size: size,
	}
}

func (n *SQLNodeIterator) Stats() graph.IteratorStats {
	size, _ := n.Size()
	return graph.IteratorStats{
		ContainsCost: 1,
		NextCost:     5,
		Size:         size,
	}
}

func (n *SQLNodeIterator) NextPath() bool {
	n.resultIndex += 1
	if n.resultIndex >= len(n.resultList) {
		return false
	}
	n.buildResult(n.resultIndex)
	return true
}

func (n *SQLNodeIterator) buildResult(i int) {
	container := n.resultList[i]
	n.result = make(map[string]string)
	for i, c := range n.cols {
		n.result[c] = container[i]
	}
}

func (n *SQLNodeIterator) getTables() []string {
	var out []string
	for _, i := range n.linkIts {
		out = append(out, i.it.getTables()...)
	}
	if len(out) == 0 {
		out = append(out, n.tableName)
	}
	return out
}

func (n *SQLNodeIterator) tableID() tagDir {
	if len(n.linkIts) == 0 {
		return tagDir{
			table: n.tableName,
			dir:   quad.Any,
		}
	}
	return tagDir{
		table: n.linkIts[0].it.tableID().table,
		dir:   n.linkIts[0].dir,
	}
}

func (n *SQLNodeIterator) getTags() []tagDir {
	myTag := n.tableID()
	var out []tagDir
	for _, tag := range n.tagger.Tags() {
		out = append(out, tagDir{
			dir:   myTag.dir,
			table: myTag.table,
			tag:   tag,
		})
	}
	for _, tag := range n.tagdirs {
		out = append(out, tagDir{
			dir:   tag.dir,
			table: myTag.table,
			tag:   tag.tag,
		})

	}
	for _, i := range n.linkIts {
		out = append(out, i.it.getTags()...)
	}
	return out
}

func (n *SQLNodeIterator) buildWhere() (string, []string) {
	var q []string
	var vals []string
	if len(n.linkIts) > 1 {
		baseTable := n.linkIts[0].it.tableID().table
		baseDir := n.linkIts[0].dir
		for _, i := range n.linkIts[1:] {
			table := i.it.tableID().table
			dir := i.dir
			q = append(q, fmt.Sprintf("%s.%s = %s.%s", baseTable, baseDir, table, dir))
		}
	}
	for _, i := range n.linkIts {
		s, v := i.it.buildWhere()
		q = append(q, s)
		vals = append(vals, v...)
	}
	query := strings.Join(q, " AND ")
	return query, vals
}

func (n *SQLNodeIterator) buildSQL(next bool, val graph.Value) (string, []string) {
	topData := n.tableID()
	query := "SELECT "
	var t []string
	t = append(t, fmt.Sprintf("%s.%s as __execd", topData.table, topData.dir))
	for _, v := range n.getTags() {
		t = append(t, fmt.Sprintf("%s.%s as %s", v.table, v.dir, v.tag))
	}
	query += strings.Join(t, ", ")
	query += " FROM "
	t = []string{}
	for _, k := range n.getTables() {
		t = append(t, fmt.Sprintf("quads as %s", k))
	}
	query += strings.Join(t, ", ")
	query += " WHERE "
	constraint, values := n.buildWhere()

	if !next {
		v := val.(string)
		if constraint != "" {
			constraint += " AND "
		}
		constraint += fmt.Sprintf("%s.%s = ?", topData.table, topData.dir)
		values = append(values, v)
	}
	query += constraint
	query += ";"

	glog.V(2).Infoln(query)

	if glog.V(4) {
		dstr := query
		for i := 1; i <= len(values); i++ {
			dstr = strings.Replace(dstr, "?", fmt.Sprintf("'%s'", values[i-1]), 1)
		}
		glog.V(4).Infoln(dstr)
	}
	return query, values
}

func (n *SQLNodeIterator) Next() bool {
	var err error
	graph.NextLogIn(n)
	if n.cursor == nil {
		err = n.makeCursor(true, nil)
		n.cols, err = n.cursor.Columns()
		if err != nil {
			glog.Errorf("Couldn't get columns")
			n.err = err
			n.cursor.Close()
			return false
		}
		// iterate the first one
		if !n.cursor.Next() {
			glog.V(4).Infoln("sql: No next")
			err := n.cursor.Err()
			if err != nil {
				glog.Errorf("Cursor error in SQL: %v", err)
				n.err = err
			}
			n.cursor.Close()
			return false
		}
		s, err := scan(n.cursor, len(n.cols))
		if err != nil {
			n.err = err
			n.cursor.Close()
			return false
		}
		n.resultNext = append(n.resultNext, s)
	}
	if n.resultList != nil && n.resultNext == nil {
		// We're on something and there's no next
		return false
	}
	n.resultList = n.resultNext
	n.resultNext = nil
	n.resultIndex = 0
	for {
		if !n.cursor.Next() {
			glog.V(4).Infoln("sql: No next")
			err := n.cursor.Err()
			if err != nil {
				glog.Errorf("Cursor error in SQL: %v", err)
				n.err = err
			}
			n.cursor.Close()
			break
		}
		s, err := scan(n.cursor, len(n.cols))
		if err != nil {
			n.err = err
			n.cursor.Close()
			return false
		}
		if n.resultList[0][0] != s[0] {
			n.resultNext = append(n.resultNext, s)
			break
		} else {
			n.resultList = append(n.resultList, s)
		}

	}
	if len(n.resultList) == 0 {
		return graph.NextLogOut(n, nil, false)
	}
	n.buildResult(0)
	return graph.NextLogOut(n, n.Result(), true)
}

func (n *SQLNodeIterator) makeCursor(next bool, value graph.Value) error {
	if n.cursor != nil {
		n.cursor.Close()
	}
	var q string
	var values []string
	q, values = n.buildSQL(next, value)
	q = convertToPostgres(q, values)
	ivalues := make([]interface{}, 0, len(values))
	for _, v := range values {
		ivalues = append(ivalues, v)
	}
	cursor, err := n.qs.db.Query(q, ivalues...)
	if err != nil {
		glog.Errorf("Couldn't get cursor from SQL database: %v", err)
		cursor = nil
		return err
	}
	n.cursor = cursor
	return nil
}

func (n *SQLNodeIterator) Contains(v graph.Value) bool {
	var err error
	//if it.preFilter(v) {
	//return false
	//}
	err = n.makeCursor(false, v)
	if err != nil {
		glog.Errorf("Couldn't make query: %v", err)
		n.err = err
		n.cursor.Close()
		return false
	}
	n.cols, err = n.cursor.Columns()
	if err != nil {
		glog.Errorf("Couldn't get columns")
		n.err = err
		n.cursor.Close()
		return false
	}
	n.resultList = nil
	for {
		if !n.cursor.Next() {
			glog.V(4).Infoln("sql: No next")
			err := n.cursor.Err()
			if err != nil {
				glog.Errorf("Cursor error in SQL: %v", err)
				n.err = err
			}
			n.cursor.Close()
			break
		}
		s, err := scan(n.cursor, len(n.cols))
		if err != nil {
			n.err = err
			n.cursor.Close()
			return false
		}
		n.resultList = append(n.resultList, s)
	}
	n.cursor.Close()
	n.cursor = nil
	if len(n.resultList) != 0 {
		n.resultIndex = 0
		n.buildResult(0)
		return true
	}
	return false
}
