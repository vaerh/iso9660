## iso9660
[![GoDoc](https://godoc.org/github.com/KarpelesLab/iso9660?status.svg)](https://godoc.org/github.com/KarpelesLab/iso9660)

A package for reading and creating ISO9660, forked from https://github.com/kdomanski/iso9660.

Requires Go 1.13 or newer.

Joliet and Rock Ridge extensions are not supported.

## Examples

### Extracting an ISO

```go
package main

import (
  "log"

  "github.com/KarpelesLab/iso9660/isoutil"
)

func main() {
  f, err := os.Open("/home/user/myImage.iso")
  if err != nil {
    log.Fatalf("failed to open file: %s", err)
  }
  defer f.Close()

  if err = isoutil.ExtractImageToDirectory(f, "/home/user/target_dir"); err != nil {
    log.Fatalf("failed to extract image: %s", err)
  }
}
```

### Creating an ISO

```go
package main

import (
  "log"
  "os"

  "github.com/KarpelesLab/iso9660"
)

func main() {
  writer, err := iso9660.NewWriter()
  if err != nil {
    log.Fatalf("failed to create writer: %s", err)
  }

  // set volume name
  writer.Primary.VolumeIdentifier = "testvol"

  err = writer.AddLocalFile("/home/user/myFile.txt", "folder/MYFILE.TXT")
  if err != nil {
    log.Fatalf("failed to add file: %s", err)
  }

  outputFile, err := os.OpenFile("/home/user/output.iso", os.O_WRONLY | os.O_TRUNC | os.O_CREATE, 0644)
  if err != nil {
    log.Fatalf("failed to create file: %s", err)
  }

  err = writer.WriteTo(outputFile)
  if err != nil {
    log.Fatalf("failed to write ISO image: %s", err)
  }

  err = outputFile.Close()
  if err != nil {
    log.Fatalf("failed to close output file: %s", err)
  }
}
```

### Streaming an ISO via HTTP

It is possible to stream a dynamically generated file on request via HTTP in order to include files or customize configuration files:

```go
import (
  "http"
  "log"

  "github.com/KarpelesLab/iso9660"
)

func ServeHTTP(rw http.RequestWriter, req *http.Request) {
  writer, err := iso9660.NewWriter()
  if err != nil {
    log.Fatalf("failed to create writer: %s", err)
  }

  // set volume name
  writer.Primary.VolumeIdentifier = "LIVE IMAGE"

  if syslinux, err := iso9660.NewItemFile("/pkg/main/sys-boot.syslinux.core/share/syslinux/isolinux.bin"); err == nil {
    writer.AddBootEntry(&iso9660.BootCatalogEntry{BootInfoTable: true}, isolinux, "isolinux/isolinux.bin")
    writer.AddLocalFile("/pkg/main/sys-boot.syslinux.core/share/syslinux/linux.c32", "isolinux/linux.c32")
    writer.AddLocalFile("/pkg/main/sys-boot.syslinux.core/share/syslinux/ldlinux.c32", "isolinux/ldlinux.c32")
  }

  writer.AddLocalFile("kernel.img", "isolinux/kernel.img")
  writer.AddLocalFile("initrd.img", "isolinux/initrd.img")
  writer.AddLocalFile("root.squashfs", "root.img")
  writer.AddFile(getSyslinuxConfig(), "isolinux/isolinux.cfg")

  rw.Header().Set("Content-Type", "application/x-iso9660-image")
  writer.WriteTo(rw)
}
```
