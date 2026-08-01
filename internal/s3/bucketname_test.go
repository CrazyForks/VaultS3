package s3

import (
	"strings"
	"testing"
)

func TestValidateBucketName(t *testing.T) {
	valid := []string{"abc", "app-data", "my.bucket.name", "0k9", strings.Repeat("a", 63)}
	for _, name := range valid {
		if err := ValidateBucketName(name); err != nil {
			t.Errorf("%q: rejected: %v", name, err)
		}
		if !isValidBucketName(name) {
			t.Errorf("%q: isValidBucketName disagrees", name)
		}
	}

	invalid := map[string]string{
		"ab":                    "between 3 and 63",
		strings.Repeat("a", 64): "between 3 and 63",
		"-lead":                 "start and end",
		"trail-":                "start and end",
		".dot":                  "start and end",
		"UPPER":                 "lowercase",
		"has_underscore":        "lowercase",
		"a..b":                  "consecutive dots",
		"../etc":                "lowercase",
	}
	for name, want := range invalid {
		err := ValidateBucketName(name)
		if err == nil {
			t.Errorf("%q: accepted, want rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q does not mention %q", name, err, want)
		}
		if isValidBucketName(name) {
			t.Errorf("%q: isValidBucketName disagrees", name)
		}
	}
}
