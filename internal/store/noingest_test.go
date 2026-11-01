package store

import (
	"testing"
	"time"
)

func TestNoIngestMarkSurvivesAndPurgeSweepsGraph(t *testing.T) {
	db := openTemp(t)
	sess := Session{ID: "secret", Source: "codex", Project: "/home/u/payroll", Path: "f", StartedAt: time.Now()}
	if err := db.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{SessionID: "secret", Seq: 0, Source: "codex", Project: "/home/u/payroll", Role: "user", Text: "merge the payroll launch plans"},
		{SessionID: "secret", Seq: 1, Source: "codex", Project: "/home/u/payroll", Role: "assistant", Text: "the payroll launch is merged"},
	}
	if err := db.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	var contributed int
	if err := db.sql.QueryRow(`SELECT count(*) FROM msg_edges WHERE session_id='secret'`).Scan(&contributed); err != nil {
		t.Fatal(err)
	}
	if contributed == 0 {
		t.Fatal("indexed session contributed no graph edges to purge")
	}

	if err := db.MarkNoIngest(sess, "private"); err != nil {
		t.Fatal(err)
	}
	marked, err := db.IsNoIngest("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !marked {
		t.Fatal("session not marked as no-ingest")
	}
	if other, err := db.IsNoIngest("other"); err != nil || other {
		t.Fatalf("uninvolved session marked: %v %v", other, err)
	}

	if err := db.PurgeSession("secret"); err != nil {
		t.Fatal(err)
	}
	sessions, messages, err := db.Count()
	if err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || messages != 0 {
		t.Fatalf("purge left %d sessions %d messages", sessions, messages)
	}
	for _, q := range []string{
		`SELECT count(*) FROM msg_edges WHERE session_id='secret'`,
		`SELECT count(*) FROM msg_terms WHERE session_id='secret'`,
		`SELECT COALESCE(sum(msgs), 0) FROM term_df`,
	} {
		var n int
		if err := db.sql.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s left %d rows", q, n)
		}
	}
	// The mark is permanent: purging the rows does not undo it.
	if marked, err := db.IsNoIngest("secret"); err != nil || !marked {
		t.Fatalf("mark lost after purge: %v %v", marked, err)
	}
}
