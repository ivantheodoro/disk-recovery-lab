//go:build !darwin

package platform

// PracticeResult describes a generated practice disk.
type PracticeResult struct {
	Image   string   `json:"image"`
	Kept    []string `json:"kept"`
	Deleted []string `json:"deleted"`
	Damaged bool     `json:"damaged"`
}

// CreatePractice needs macOS disk-image tools.
func CreatePractice(dir, kind string) (*PracticeResult, error) { return nil, ErrUnsupported }
