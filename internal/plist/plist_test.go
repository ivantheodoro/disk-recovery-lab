package plist

import "testing"

func TestDecode(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>AllDisks</key><array><string>disk0</string><string>disk0s1</string></array>
  <key>Size</key><integer>1000204886016</integer>
  <key>Internal</key><true/>
  <key>Removable</key><false/>
  <key>Ratio</key><real>0.5</real>
  <key>Blob</key><data>aGk=</data>
</dict></plist>`
	v, err := Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	m := Dict(v)
	if Int(m["Size"]) != 1000204886016 || !Bool(m["Internal"]) || Bool(m["Removable"]) ||
		Str(Arr(m["AllDisks"])[1]) != "disk0s1" || m["Ratio"].(float64) != 0.5 || string(m["Blob"].([]byte)) != "hi" {
		t.Fatalf("unexpected: %#v", m)
	}
}
