package s3

import (
	"encoding/xml"
	"net/http"
	"testing"
)

// TestListObjectsV2FolderHasDate covers issue #35: a folder (CommonPrefix) in a
// delimited listing carries a LastModified sourced from the folder's contents, so
// folders don't list dateless (which makes clients fake a date).
func TestListObjectsV2FolderHasDate(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "folderdate"
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil).Body.Close()
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/docs/a.txt", []byte("x")).Body.Close()
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/docs/b.txt", []byte("y")).Body.Close()

	resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"?list-type=2&delimiter=/", nil)
	body := readBody(t, resp)

	var parsed struct {
		CommonPrefixes []struct {
			Prefix       string `xml:"Prefix"`
			LastModified string `xml:"LastModified"`
		} `xml:"CommonPrefixes"`
	}
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse: %v (%s)", err, body)
	}
	if len(parsed.CommonPrefixes) != 1 || parsed.CommonPrefixes[0].Prefix != "docs/" {
		t.Fatalf("common prefixes = %+v, want one docs/ (%s)", parsed.CommonPrefixes, body)
	}
	if parsed.CommonPrefixes[0].LastModified == "" {
		t.Fatalf("folder has no LastModified: %s", body)
	}
}
