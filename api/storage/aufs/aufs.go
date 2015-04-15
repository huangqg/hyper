package aufs

import (
    "bufio"
    "io/ioutil"
    "os"
    "path"
    "syscall"
    "sync"
)

/*
|-- layers  // Metadata of layers
|   |---- 1
|   |---- 2
|   |---- 3
|-- diff    // Content of the layer
|   |---- 1
|   |---- 2
|   |---- 3
|-- mnt     // Mount points for the rw layers to be mounted
    |---- 1
    |---- 2
    |---- 3
*/

var (
    enableDirpermLock sync.Once
    enableDirperm     bool
)

const MsRemount = syscall.MS_REMOUNT

func mountContainerToSharedDir(containerId, backedFs, rootDir, sharedDir string) (string, error) {
    var (
        mntPath = path.Join(rootDir, "mnt")
        layersPath = path.Join(rootDir, "layers")
        diffPath = path.Join(rootDir, "diff")
        mountPoint = path.Join(sharedDir, containerId, "rootfs")
    )

	layers, err := getParentDiffPaths(containerId, rootDir)
	if err != nil {
		return "", err
	}

	if err := aufsMount(layers, path.Join(diffPath, containerId), mountPoint, mountLabel); err != nil {
		return "", fmt.Errorf("DVM ERROR: error creating aufs mount to %s: %v", target, err)
	}

    return mountPoint, nil
}

func attachFiles(containerId, fromFile, toDir, rootDir, perm string) (string, error) {
    return "", nil
}

func createVolume(backedFs, rootDir string) (string, error) {
    return "", nil
}

func getParentDiffPaths(id, rootPath string) ([]string, error) {
	parentIds, err := getParentIds(path.Join(rootPath, "layers", id))
	if err != nil {
		return nil, err
	}
	layers := make([]string, len(parentIds))

	// Get the diff paths for all the parent ids
	for i, p := range parentIds {
		layers[i] = path.Join(rootPath, "diff", p)
	}
	return layers, nil
}

func aufsMount(ro []string, rw, target, mountLabel string) (err error) {
	defer func() {
		if err != nil {
			aufsUnmount(target)
		}
	}()

	// Mount options are clipped to page size(4096 bytes). If there are more
	// layers then these are remounted individually using append.

	offset := 54
	if useDirperm() {
		offset += len("dirperm1")
	}
	b := make([]byte, syscall.Getpagesize()-len(mountLabel)-offset) // room for xino & mountLabel
	bp := copy(b, fmt.Sprintf("br:%s=rw", rw))

	firstMount := true
	i := 0

	for {
		for ; i < len(ro); i++ {
			layer := fmt.Sprintf(":%s=ro+wh", ro[i])

			if firstMount {
				if bp+len(layer) > len(b) {
					break
				}
				bp += copy(b[bp:], layer)
			} else {
				data := label.FormatMountLabel(fmt.Sprintf("append%s", layer), mountLabel)
				if err = syscall.Mount("none", target, "aufs", MsRemount, data); err != nil {
					return
				}
			}
		}

		if firstMount {
			opts := "dio,xino=/dev/shm/aufs.xino"
			if useDirperm() {
				opts += ",dirperm1"
			}
			data := label.FormatMountLabel(fmt.Sprintf("%s,%s", string(b[:bp]), opts), mountLabel)
			if err = syscall.Mount("none", target, "aufs", 0, data); err != nil {
				return
			}
			firstMount = false
		}

		if i == len(ro) {
			break
		}
	}

	return
}

// useDirperm checks dirperm1 mount option can be used with the current
// version of aufs.
func useDirperm() bool {
	enableDirpermLock.Do(func() {
		base, err := ioutil.TempDir("", "docker-aufs-base")
		if err != nil {
			fmt.Printf("DVM ERROR: error checking dirperm1: %s", err.Error())
			return
		}
		defer os.RemoveAll(base)

		union, err := ioutil.TempDir("", "docker-aufs-union")
		if err != nil {
			fmt.Printf("DVM ERROR: error checking dirperm1: %s", err.Error())
			return
		}
		defer os.RemoveAll(union)

		opts := fmt.Sprintf("br:%s,dirperm1,xino=/dev/shm/aufs.xino", base)
		if err := syscall.Mount("none", union, "aufs", 0, opts); err != nil {
			return
		}
		enableDirperm = true
		if err := aufsUnmount(union); err != nil {
			fmt.Printf("DVM ERROR: error checking dirperm1: failed to unmount %s", err.Error())
		}
	})
	return enableDirperm
}

func aufsUnmount(target string) error {
    if err := exec.Command("auplink", target, "flush").Run(); err != nil {
        fmt.Printf("Couldn't run auplink before unmount: %s", err.Error())
    }
    if err := syscall.Unmount(target, 0); err != nil {
        return err
    }
    return nil
}

// Return all the directories
func loadIds(root string) ([]string, error) {
    dirs, err := ioutil.ReadDir(root)
    if err != nil {
        return nil, err
    }
    out := []string{}
    for _, d := range dirs {
        if !d.IsDir() {
            out = append(out, d.Name())
        }
    }
    return out, nil
}

// Read the layers file for the current id and return all the
// layers represented by new lines in the file
//
// If there are no lines in the file then the id has no parent
// and an empty slice is returned.
func getParentIds(id string) ([]string, error) {
    f, err := os.Open(id)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    out := []string{}
    s := bufio.NewScanner(f)

    for s.Scan() {
        if t := s.Text(); t != "" {
            out = append(out, s.Text())
        }
    }
    return out, s.Err()
}