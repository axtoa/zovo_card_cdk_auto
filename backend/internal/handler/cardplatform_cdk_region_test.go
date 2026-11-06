package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestCDKListRegionRoundTripAndPageRefresh(t *testing.T) {
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	old := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = old; conn.Close() })
	check := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	for _, q := range []string{
		`CREATE TABLE site_settings(key TEXT PRIMARY KEY,value TEXT,updated_at DATETIME)`,
		`CREATE TABLE cardplatform_cdk_codes(account_id INTEGER NOT NULL DEFAULT 1,upstream_id INTEGER,code TEXT UNIQUE NOT NULL,code_prefix TEXT,plan TEXT,fee_amount_minor INTEGER,status TEXT,payment_country TEXT,created_at DATETIME)`,
		`CREATE TABLE cardplatform_cdk_notes(account_id INTEGER NOT NULL DEFAULT 1,upstream_id INTEGER,note TEXT,PRIMARY KEY(account_id,upstream_id))`,
	} {
		_, e := conn.Exec(q)
		check(e)
	}
	for id := int64(1); id <= 3; id++ {
		check(db.ForAccount(1).SaveCardplatformCDKCode(id, "ZC-SYNTHETIC-CODE-"+strconv.FormatInt(id, 10), "", "plus", 15))
	}
	check(db.ForAccount(1).UpdateCardplatformCDKRegion(3, "US"))
	var mu sync.Mutex
	queries := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/openapi/v1/gpt-direct/cdks" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		q := r.URL.Query().Get("q")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		if q == "3" {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"code":503,"msg":"test unavailable"}`))
			return
		}
		country := "CL"
		id := int64(1)
		if q == "2" {
			country = ""
			id = 2
		}
		list := []map[string]any{{"id": 999, "plan": "plus", "payment_country": "JP", "status": "unused"}, {"id": id, "plan": "plus", "payment_country": country, "status": "unused", "fee_amount_minor": 15}}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": list, "total": 2}})
	}))
	t.Cleanup(upstream.Close)
	check(db.SetSetting("card_api_base", upstream.URL))
	check(db.SetSetting("card_api_key", "synthetic-test-key"))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(cardplatform.WithAccount(c.Request.Context(), db.CardAccount{ID: 1, APIKey: "synthetic-test-key"}))
		c.Next()
	})
	router.GET("/stored", CardPlatformListStoredCDKs)
	router.GET("/upstream", CardPlatformListCDKs)
	get := func(path string) []map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var body struct {
			List []map[string]any `json:"list"`
		}
		check(json.Unmarshal(w.Body.Bytes(), &body))
		return body.List
	}
	rows := get("/stored?page=1&page_size=3&sync=0")
	for _, r := range rows {
		if r["id"] == float64(1) && r["payment_country"] != nil {
			t.Fatal("unknown became PH")
		}
	}
	rows = get("/stored?page=1&page_size=3")
	for _, r := range rows {
		id := int64(r["id"].(float64))
		want := map[int64]string{1: "CL", 2: "", 3: "US"}[id]
		if r["payment_country"] != want {
			t.Fatalf("id=%d country=%v want=%s", id, r["payment_country"], want)
		}
	}
	mu.Lock()
	if len(queries) != 3 {
		t.Errorf("extra per-code requests: %v", queries)
	}
	mu.Unlock()
	rows = get("/stored?page=1&page_size=3&sync=0")
	for _, r := range rows {
		if r["id"] == float64(1) && r["payment_country"] != "CL" {
			t.Fatal("refresh not persisted")
		}
	}
	rows = get("/upstream?page=1&page_size=20")
	if rows[1]["payment_country"] != "CL" {
		t.Fatal("upstream list dropped country")
	}
}
