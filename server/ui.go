package server

import (
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	raven "github.com/getsentry/raven-go"
	"github.com/julienschmidt/httprouter"
)

// A SimpleItem is like an Item, but does not contain the blob and version information.
// It is used to simplify the item list displays.
type SimpleItem struct {
	ID        string
	MaxBundle int // largest bundle id used by this item
	Created   time.Time
	Modified  time.Time
	Size      int64
}

// UIItemsHandler handles requests from GET /ui/items
func (s *RESTServer) UIItemsHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	n := 0
	p := 1000
	sort := "-modified"

	if option := r.FormValue("n"); option != "" {
		offset, err := strconv.Atoi(option)
		if err == nil && offset >= 0 {
			n = offset
		}
	}

	if option := r.FormValue("p"); option != "" {
		pagesize, err := strconv.Atoi(option)
		if err == nil && pagesize > 0 && pagesize < 2000 {
			p = pagesize
		}
	}

	if option := r.FormValue("s"); option != "" {
		// only allow search options we recognize
		switch option {
		case "name", "-name", "size", "-size",
			"modified", "-modified", "created", "-created":
			sort = option
		}
	}

	items, err := s.BlobDB.GetItemList(n, p, sort)
	if err != nil {
		log.Println(err)
		raven.CaptureError(err, nil)
	}

	results := struct {
		N     int
		NextN int
		PrevN int
		P     int
		Sort  string
		Items []SimpleItem
	}{
		N:     n,
		NextN: n + p,
		P:     p,
		Sort:  sort,
		Items: items,
	}
	// only need to set if the previous page will be > 0
	if n > p {
		results.PrevN = n - p
	}

	err = itemlistTemplate.Execute(w, results)
	if err != nil {
		log.Println(err)
		raven.CaptureError(err, nil)
	}
}

func nextSort(goalsort, currentsort string) string {
	if goalsort == currentsort {
		return "-" + goalsort
	}
	return goalsort
}

var (
	itemlistfns = template.FuncMap{
		"nextsort": nextSort,
	}

	itemlistTemplate = template.Must(template.New("itemlist").Funcs(itemlistfns).Parse(`
<html><head><style>
tbody tr:nth-child(even) { background-color: #eeeeee; }
</style></head><body>
<h1>Item List</h1>

<dl>
	<dt>Start Offset</dt><dd>{{ .N }}</dd>
	<dt>Items per page</dt><dd>{{ .P }}</dd>
	<dt>Sort</dt><dd>{{ .Sort }}</dd>
</dl>

<a href="?p={{ .P }}&n={{ .PrevN }}&s={{ .Sort }}">Previous Page</a>
•
<a href="?p={{ .P }}&n={{ .NextN }}&s={{ .Sort }}">Next Page</a>

<table><thead><tr>
	<th><a href="?p={{ .P }}&s={{ nextsort "name" .Sort }}">Item</a></th>
	<th><a href="?p={{ .P }}&s={{ nextsort "created" .Sort }}">Date Created</a></th>
	<th><a href="?p={{ .P }}&s={{ nextsort "modified" .Sort }}">Date Modified</a></th>
	<th><a href="?p={{ .P }}&s={{ nextsort "size" .Sort }}">Size</a></th>
</tr></thead><tbody>
{{ range .Items }}
	<tr>
		<td><a href="/item/{{ .ID }}">{{ .ID }}</a></td>
		<td>{{ .Created }}</td>
		<td>{{ .Modified }}</td>
		<td>{{ .Size }}</td>
	</tr>
{{ end }}
</tbody></table>
</body></html>`))
)
