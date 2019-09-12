package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	ignore "github.com/codeskyblue/dockerignore"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
)

var (
	fSrc  = flag.String("src", "/src", "path with canonical files")
	fDest = flag.String("dest", "/dest", "path to sync data to")
	fIgn  = flag.String("ignore", "", "file with patterns to ignore")
)

var ignorePatterns []string

func main() {
	flag.Parse()

	var err error
	if *fIgn != "" {
		ignorePatterns, err = ignore.ReadIgnoreFile(*fIgn)
		if err != nil {
			log.Fatal(err)
		}
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	statusPath := filepath.Join(*fDest, ".synced")

	os.Remove(statusPath)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	defer w.Close()

	err = w.Add(*fSrc)
	if err != nil {
		return err
	}

	cancel := make(chan os.Signal, 1)

	signal.Notify(cancel, os.Interrupt)

	err = syncDirs(w, cancel)
	if err != nil {
		return err
	}

	// Touch the status path to tell others it's ready
	f, err := os.Create(statusPath)
	if err == nil {
		f.Close()
	}

	log.Printf("Watching for events")

	for {
		select {
		case <-cancel:
			return nil
		case err := <-w.Errors:
			return err
		case ev := <-w.Events:
			rel, err := filepath.Rel(*fSrc, ev.Name)
			if err != nil {
				return err
			}

			if match, err := ignore.Matches(rel, ignorePatterns); err == nil && match {
				return nil
			}

			if ev.Op&fsnotify.Create == fsnotify.Create {
				if err = createFile(rel); err != nil {
					return err
				}
			}

			if ev.Op&fsnotify.Write == fsnotify.Write {
				if err = copyFile(rel, true, 0); err != nil {
					return err
				}
			}

			if ev.Op&fsnotify.Remove == fsnotify.Remove {
				if err = removeFile(rel); err != nil {
					return err
				}
			}

			if ev.Op&fsnotify.Chmod == fsnotify.Chmod {
				if err = chmodFile(rel); err != nil {
					return err
				}
			}
		}
	}
}

func syncDirs(w *fsnotify.Watcher, cancel chan os.Signal) error {
	log.Printf("Performing initial sync")

	var total int64
	var nprint int

	err := filepath.Walk(*fSrc, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		select {
		case <-cancel:
			return fmt.Errorf("canceled")
		default:
		}

		rel, err := filepath.Rel(*fSrc, path)
		if err != nil {
			return errors.Wrapf(err, "calculating rel path")
		}

		if match, err := ignore.Matches(rel, ignorePatterns); err == nil && match {
			if fi.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		to := filepath.Join(*fDest, rel)

		if fi.IsDir() {
			if nprint == 0 {
				log.Printf("=> %s", path)
				nprint++
			} else {
				nprint++
				if nprint == 100 {
					nprint = 0
				}
			}

			w.Add(path)
			ft, err := os.Stat(to)
			if err != nil {
				if os.IsNotExist(err) {
					err = os.Mkdir(to, fi.Mode())
					if err != nil {
						return errors.Wrapf(err, "making a directory")
					}

					return nil
				}
				return errors.Wrapf(err, "error stating")
			}

			if !ft.IsDir() {
				err = os.Remove(to)
				if err != nil {
					return errors.Wrapf(err, "removing errant non-dir")
				}

				err = os.Mkdir(to, fi.Mode())
				if err != nil {
					return errors.Wrapf(err, "making a directory")
				}
			} else {
				err = os.Chmod(to, fi.Mode())
				if err != nil {
					return errors.Wrapf(err, "chmod")
				}
			}
		}

		if !fi.Mode().IsRegular() {
			if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
				lnk, err := os.Readlink(path)
				if err != nil {
					return errors.Wrapf(err, "reading link from %s", path)
				}

				os.Remove(to)

				err = os.Symlink(lnk, to)
				if err != nil {
					return errors.Wrapf(err, "symlinking")
				}
			}

			return nil
		}

		if tfi, err := os.Stat(to); err == nil {
			// We're expending a regular file and ergo if the dest is not a regular file, remove it.
			if !tfi.Mode().IsRegular() {
				err = os.RemoveAll(to)
				if err != nil {
					return err
				}
			} else if tfi.Size() == fi.Size() && tfi.ModTime().After(fi.ModTime()) || tfi.ModTime().Equal(fi.ModTime()) {
				return nil
			}
		}

		total += fi.Size()
		err = copyFile(rel, false, os.O_CREATE)
		if err != nil {
			return errors.Wrapf(err, "copying file")
		}

		return nil
	})

	if err != nil {
		return err
	}

	log.Printf("Initial sync done: %d bytes", total)

	return nil
}

func createFile(rel string) error {
	to := filepath.Join(*fDest, rel)

	fi, err := os.Create(to)
	if err != nil {
		return err
	}

	log.Printf("Created %s", rel)
	return fi.Close()
}

func copyFile(rel string, stat bool, flag int) error {
	var (
		from = filepath.Join(*fSrc, rel)
		to   = filepath.Join(*fDest, rel)
	)

	ff, err := os.Open(from)
	if err != nil {
		return err
	}

	fi, err := ff.Stat()
	if err != nil {
		return err
	}

	switch fi.Mode() & os.ModeType {
	case os.ModeDevice, os.ModeCharDevice:
		log.Printf("Cowardly refusing to copy devices")
		return nil
	case os.ModeNamedPipe:
		log.Printf("Cowardly refusing to copy named pipe")
		return nil
	case os.ModeSocket:
		log.Printf("Cowardly refusing to copy socket")
		return nil
	case os.ModeDir:
		log.Printf("Cowardly refusing to copy directory")
		return nil
	case os.ModeSymlink, 0:
		// symlink or regular, that's fine
	default:
		log.Printf("Cowardly refusing to copy unknown file type: %d", fi.Mode()&os.ModeType)
		return nil
	}

	tf, err := os.OpenFile(to, os.O_TRUNC|os.O_WRONLY|flag, fi.Mode())
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Unable to copy to %s, doesn't exist", rel)
			return nil
		}

		return errors.Wrapf(err, "opening file for writing")
	}

	// Skip where the from is size 0, ie a lock file
	if fi.Size() == 0 {
		log.Printf("File %s is 0 bytes, truncating", rel)
		return tf.Close()
	}

	if stat {
		log.Printf("Copying %s (%d bytes)", rel, fi.Size())
	}

	start := time.Now()

	_, err = io.Copy(tf, ff)
	if err != nil {
		return err
	}

	if stat {
		log.Printf(" Copied %s (%s elapsed)", rel, time.Since(start))
	}

	return nil
}

func removeFile(rel string) error {
	log.Printf("Remove %s", rel)
	os.Remove(filepath.Join(*fDest, rel))
	return nil
}

func chmodFile(rel string) error {
	var (
		from = filepath.Join(*fSrc, rel)
		to   = filepath.Join(*fDest, rel)
	)

	fi, err := os.Stat(from)
	if err != nil {
		return err
	}

	log.Printf("Chmod %s (%s)", rel, fi.Mode())

	return os.Chmod(to, fi.Mode())
}
