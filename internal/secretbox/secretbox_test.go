package secretbox

import "testing"

func TestRoundTrip(t *testing.T) {
	box, err := New("test-master-key")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("sensitive")
	if err != nil {
		t.Fatal(err)
	}
	if sealed == "sensitive" {
		t.Fatal("secret was stored as plaintext")
	}
	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened != "sensitive" {
		t.Fatalf("opened = %q", opened)
	}
}

func TestRequiresKey(t *testing.T) {
	if _, err := New(""); err != ErrKeyRequired {
		t.Fatalf("err = %v, want ErrKeyRequired", err)
	}
}
