package main

import (
	"testing"
)

func TestContains(t *testing.T) {
	if Contains([]string{"a", "b"}, "b") == false {
		t.Error("'b' not found.")
	}
	if Contains([]string{"a", "b"}, "aa") == true {
		t.Error("'aa' found. weird.")
	}
}

func TestAtoi(t *testing.T) {
	// test success
	if i := Atoi("123", 17); i != 123 {
		t.Errorf("Failed to run Atoi on \"123\". Got %v instead.", i)
	}
	// test default
	if i := Atoi("blah", 17); i != 17 {
		t.Errorf("Failed to run Atoi on \"\". Got %v instead.", i)
	}
	// test default
	if i := Atoi("", 17); i != 17 {
		t.Errorf("Failed to run Atoi on \"\". Got %v instead.", i)
	}
}

func TestFindMissing(t *testing.T) {
	a := []string{"bar", "foo", "tar"}
	b := []string{"foo", "com"}
	c := FindMissing(a, b)
	t.Logf("result: %+v", c)
	if len(c) != 2 {
		t.Fatalf("Result size differs. Expected 2 but got %v", len(c))
	}
	if c[0] != "bar" {
		t.Errorf("\"bar\" not found, but \"%v\".", c[0])
	}
	if c[1] != "tar" {
		t.Errorf("\"tar\" not found, but \"%v\".", c[1])
	}
}
