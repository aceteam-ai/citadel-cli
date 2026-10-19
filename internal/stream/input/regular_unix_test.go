//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package input

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRegularRefusesFIFOWithoutWaitingForWriter(t *testing.T) {
	root, dir := testRoot(t)
	path := filepath.Join(dir,"input.log")
	if err := unix.Mkfifo(path,0600); err != nil { t.Fatal(err) }
	if file, err := OpenRegular(root,"input.log"); !errors.Is(err,ErrNotRegular) || file != nil { if file != nil { file.Close() }; t.Fatalf("FIFO selection accepted: %v",err) }
}

func TestOpenRegularRefusesDeterministicRegularToFIFORaceWithoutBlocking(t *testing.T) {
	root, dir := testRoot(t)
	path := filepath.Join(dir,"input.log")
	if err := os.WriteFile(path,[]byte("regular before open"),0600); err != nil { t.Fatal(err) }
	result := make(chan error,1)
	opening := make(chan struct{})
	go func(){
		file, err := openRegular(root,"input.log",func(root *os.Root,name string)(*os.File,error){
			if err := os.Rename(path,filepath.Join(dir,"original.log")); err != nil { close(opening); return nil,err }
			if err := unix.Mkfifo(path,0600); err != nil { close(opening); return nil,err }
			close(opening)
			return openRead(root,name)
		})
		if file != nil { file.Close() }
		result <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(),2*time.Second)
	defer cancel()
	select {
	case err := <-result:
		if !errors.Is(err,ErrNotRegular) { t.Fatalf("FIFO replacement was not explicitly refused: %v",err) }
	case <-ctx.Done():
		// If the nonblocking flag regresses, release a blocked reader before
		// failing so the test does not deliberately strand an open goroutine.
		select { case <-opening: default: }
		writer, err := unix.Open(path,unix.O_WRONLY|unix.O_NONBLOCK,0)
		if err == nil { unix.Close(writer) }
		select { case <-result: case <-time.After(2*time.Second): t.Error("FIFO open did not unwind after a writer was supplied") }
		t.Fatal("regular-to-FIFO replacement blocked waiting for a writer")
	}
}
