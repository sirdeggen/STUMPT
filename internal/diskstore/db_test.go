package diskstore

import "testing"

func TestDBOpenClose(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if db == nil {
		t.Fatal("expected non-nil DB")
	}
	if db.BadgerDB() == nil {
		t.Fatal("expected non-nil BadgerDB")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDBDoubleClose(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Second close should not panic.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
