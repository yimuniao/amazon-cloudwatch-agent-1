// +build linux darwin

package tail

import (
	"github.com/stretchr/testify/require"
	"io/ioutil"
	"os"
	"testing"
)

func TestTail_isFileDeleted(t *testing.T) {
	tail := &Tail{}
	file, err := ioutil.TempFile("/tmp", "test_deletion")
	require.NoError(t, err)
	file.Close()

	tail.Filename = file.Name()
	tail.file, err = OpenFile(tail.Filename)
	if err != nil {
		t.Errorf("failed to open file  = %v", tail.Filename)
	}

	if tail.isFileDeleted() != false {
		t.Errorf("Tail file isFileDeleted should be %v", false)
	}
	os.Remove(file.Name())
	if tail.isFileDeleted() != true {
		t.Errorf("Tail file isFileDeleted should be %v", true)
	}
}
