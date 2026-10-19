package input

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func testRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func(){ root.Close() })
	return root, dir
}

func TestOpenRegularReadsReadOnlySelectedFile(t *testing.T) {
	root, dir := testRoot(t)
	if err := os.WriteFile(filepath.Join(dir,"selected.log"),[]byte("synthetic\n"),0600); err != nil { t.Fatal(err) }
	file, err := OpenRegular(root,"selected.log")
	if err != nil { t.Fatal(err) }
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "synthetic\n" { t.Fatalf("selected bytes changed: %q %v",data,err) }
	if _, err := file.Write([]byte("must not write")); err == nil { t.Fatal("input descriptor was writable") }
}

func TestOpenRegularRejectsDirectoryMissingAndRootEscape(t *testing.T) {
	root, dir := testRoot(t)
	if err := os.Mkdir(filepath.Join(dir,"directory"),0700); err != nil { t.Fatal(err) }
	outside := filepath.Join(t.TempDir(),"outside.log")
	if err := os.WriteFile(outside,[]byte("outside"),0600); err != nil { t.Fatal(err) }
	for _, name := range []string{"directory","missing.log","../outside.log",outside} {
		if file, err := OpenRegular(root,name); err == nil || file != nil { if file != nil { file.Close() }; t.Fatalf("non-regular or escaping selection accepted: %q %v",name,err) }
	}
	if file, err := OpenRegular(nil,"input.log"); !errors.Is(err,ErrNotRegular) || file != nil { t.Fatalf("nil root accepted: %v",err) }
}

func TestOpenRegularRejectsSelectedSymlink(t *testing.T) {
	root, dir := testRoot(t)
	if err := os.WriteFile(filepath.Join(dir,"regular.log"),[]byte("synthetic"),0600); err != nil { t.Fatal(err) }
	if err := os.Symlink("regular.log",filepath.Join(dir,"linked.log")); err != nil { t.Skip("symlink creation unavailable on this platform") }
	if file, err := OpenRegular(root,"linked.log"); !errors.Is(err,ErrNotRegular) || file != nil { if file != nil { file.Close() }; t.Fatalf("selected symlink accepted: %v",err) }
}

func TestOpenRegularRejectsDifferentRegularReplacementAndClosesDescriptor(t *testing.T) {
	root, dir := testRoot(t)
	selected := filepath.Join(dir,"input.log")
	if err := os.WriteFile(selected,[]byte("original"),0600); err != nil { t.Fatal(err) }
	var opened *os.File
	file, err := openRegular(root,"input.log",func(root *os.Root,name string)(*os.File,error){
		if err := os.Rename(selected,filepath.Join(dir,"original.log")); err != nil { return nil,err }
		if err := os.WriteFile(selected,[]byte("replacement"),0600); err != nil { return nil,err }
		var err error
		opened, err = openRead(root,name)
		return opened,err
	})
	if !errors.Is(err,ErrNotRegular) || file != nil { if file != nil { file.Close() }; t.Fatalf("replacement inode accepted: %v",err) }
	if opened == nil { t.Fatal("test did not reach descriptor verification") }
	if _, err := opened.Stat(); !errors.Is(err,os.ErrClosed) { t.Fatalf("rejected descriptor was not closed: %v",err) }
}
