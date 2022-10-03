package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	cli "github.com/urfave/cli/v2"
)

func main() {
	app := cli.NewApp()

	app.Commands = []*cli.Command{
		receiveCmd,
		sendCmd,
	}

	app.RunAndExitOnError()
}

var receiveCmd = &cli.Command{
	Name: "receive",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "listen",
			Value: ":4900",
		},
	},
	Action: func(cctx *cli.Context) error {
		dir := cctx.Args().First()
		if dir == "" {
			curwd, err := os.Getwd()
			if err != nil {
				return err
			}

			dir = curwd
		}

		list, err := net.Listen("tcp", cctx.String("listen"))
		if err != nil {
			return err
		}

		// First connection is the control channel
		control, err := list.Accept()
		if err != nil {
			return err
		}

		var wg sync.WaitGroup

		// The rest are just data channels
		go func() {
			defer list.Close()
			for {
				con, err := list.Accept()
				if err != nil {
					fmt.Println("failed to accept new connection: ", err)
					return
				}

				fmt.Println("accepted new data connection")
				wg.Add(1)
				go func(cc net.Conn) {
					if err := handleReceivingFiles(dir, cc, &wg); err != nil {
						fmt.Println("handleReceivingFiles errored: ", err)
					}
				}(con)
			}
		}()

		spkt, err := readControlPacket(control)
		if err != nil {
			return err
		}

		if spkt.Event != EventStart {
			return fmt.Errorf("first control packet should be start announcement")
		}

		select {}
		fmt.Println("now waiting on control end")
		spkt, err = readControlPacket(control)
		if err != nil {
			return fmt.Errorf("failed to read control end packet: %w", err)
		}

		fmt.Println("ending...")
		wg.Wait()
		fmt.Println("complete!")

		return nil
	},
}

func handleReceivingFiles(root string, con net.Conn, wg *sync.WaitGroup) error {
	defer wg.Done()
	for {
		dec := json.NewDecoder(con)
		pkt, err := readControlPacket(con)
		if err != nil {
			return fmt.Errorf("failed to read control packet: %w", err)
		}
		fmt.Println("got control packet")
		r := io.MultiReader(dec.Buffered(), con)

		if pkt.Event != EventFile {
			fmt.Printf("%#v\n", pkt)
			return fmt.Errorf("got non-file event on data socket: %d", pkt.Event)
		}

		fh := pkt.File
		switch {
		case fh.IsDir:
			if err := os.Mkdir(filepath.Join(root, fh.Path, fh.Name), fh.Perms); err != nil {
				return err
			}
		case fh.IsLink:
			lval, err := ioutil.ReadAll(io.LimitReader(r, fh.Size))
			if err != nil {
				return fmt.Errorf("reading link data: %w", err)
			}
			if err := os.Symlink(string(lval), filepath.Join(root, fh.Path, fh.Name)); err != nil {
				return err
			}
		default:
			if err := handleFileTransfer(r, root, fh); err != nil {
				return fmt.Errorf("handling incoming file: %w", err)
			}

		}
	}

}

func handleFileTransfer(r io.Reader, root string, fh *fileHeader) error {
	fi, err := os.OpenFile(filepath.Join(root, fh.Path, fh.Name), os.O_RDWR|os.O_CREATE|os.O_TRUNC, fh.Perms)
	if err != nil {
		return err
	}
	defer fi.Close()

	n, err := io.CopyN(fi, r, fh.Size)
	if err != nil {
		return err
	}

	if n != fh.Size {
		return fmt.Errorf("failed to copy the right amount of bytes: %d != %d", n, fh.Size)
	}

	return nil
}

const (
	EventStart = 1
	EventEnd   = 2
	EventFile  = 3
)

type fileHeader struct {
	Name    string
	Path    string
	Perms   os.FileMode
	ModTime time.Time
	IsDir   bool
	IsLink  bool
	Size    int64
}

type controlPacket struct {
	Event int
	File  *fileHeader
}

func readControlPacket(con net.Conn) (*controlPacket, error) {
	msize := make([]byte, 4)
	_, err := io.ReadFull(con, msize)
	if err != nil {
		return nil, err
	}
	l := binary.BigEndian.Uint32(msize)
	rr := io.LimitReader(con, int64(l))

	var pkt controlPacket
	dec := json.NewDecoder(rr)
	if err := dec.Decode(&pkt); err != nil {
		return nil, err
	}

	return &pkt, nil
}

func writeControlPacket(con net.Conn, pkt *controlPacket) error {
	b, err := json.Marshal(pkt)
	if err != nil {
		return err
	}
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	con.Write(buf)
	_, err = con.Write(b)
	return err
}

var sendCmd = &cli.Command{
	Name: "send",
	Flags: []cli.Flag{
		&cli.IntFlag{
			Name:  "concurrency",
			Value: 1,
		},
	},
	Action: func(cctx *cli.Context) error {
		if !cctx.Args().Present() {
			return fmt.Errorf("must specify directory to send")
		}

		root := cctx.Args().First()

		dest := cctx.Args().Get(1)

		con, err := net.Dial("tcp", dest)
		if err != nil {
			return err
		}

		if err := writeControlPacket(con, &controlPacket{Event: EventStart}); err != nil {
			return err
		}

		datacon, err := net.Dial("tcp", dest)
		if err != nil {
			return err
		}

		// TODO: there are more efficient ways of walking the filesystem
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if filepath.Clean(root) == filepath.Clean(path) {
				return nil
			}

			fmt.Printf("sending: %q\n", path)
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			name := filepath.Base(rel)
			dir := filepath.Dir(rel)

			info, err := d.Info()
			if err != nil {
				return err
			}

			islink := (info.Mode() & fs.ModeSymlink) > 0

			if err := writeControlPacket(datacon, &controlPacket{
				Event: EventFile,
				File: &fileHeader{
					Name:    name,
					Path:    dir,
					Perms:   info.Mode().Perm(),
					ModTime: info.ModTime(),
					IsDir:   d.IsDir(),
					IsLink:  islink,
					Size:    info.Size(),
				},
			}); err != nil {
				return err
			}

			if islink {
				return fmt.Errorf("cant handle symlinks right now")
			}

			if d.IsDir() {
				fmt.Println("is dir!")
				return nil
			}

			if err := sendFile(datacon, path, info.Size()); err != nil {
				return fmt.Errorf("sending file: %w", err)
			}

			return nil
		}); err != nil {
			return err
		}

		fmt.Println("terminating...")
		return nil
	},
}

func sendFile(con net.Conn, path string, size int64) error {
	fi, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fi.Close()

	n, err := io.CopyN(con, fi, size)
	if err != nil {
		return fmt.Errorf("sending file data: %w", err)
	}

	if n != size {
		return fmt.Errorf("expected %d bytes, got %d", size, n)
	}

	return nil
}
