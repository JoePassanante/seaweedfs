package command

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

const (
	defaultFnFormat = `{{.Id}}_{{.Name}}{{.Ext}}`
	timeFormat      = "2006-01-02T15:04:05"
	listHeader      = "key\tname\tsize\tgzip\tmime\tmodified\tttl\tdeleted\tstart\tstop\n"
)

var (
	export ExportOptions
)

type ExportOptions struct {
	dir        *string
	collection *string
	volumeId   *int
}

var cmdExport = &Command{
	UsageLine: "export -dir=/tmp -volumeId=234 -o=/dir/name.tar -fileNameFormat={{.Name}} -newer='" + timeFormat + "'",
	Short:     "list or export files from one volume data file",
	Long: `List all files in a volume, or Export all files in a volume to a tar file if the output is specified.

	The format of file name in the tar file can be customized. Default is {{.Mime}}/{{.Id}}:{{.Name}}. Also available is {{.Key}}.

	With -fid, only that file is exported. It is read at the offset recorded in the .idx file, without
	scanning the volume, and its content is written to the file -o names:

		weed export -dir=/tmp -fid=234,01637037d6 -o=/dir/name.jpg

	Without -o it is written to the current directory, named after the file id, e.g. 234,01637037d6.jpg.
	An -o ending with .tar still writes a tar file, holding the one file.

  `,
}

func init() {
	cmdExport.Run = runExport // break init cycle
	export.dir = cmdExport.Flag.String("dir", ".", "input data directory to store volume data files")
	export.collection = cmdExport.Flag.String("collection", "", "the volume collection name")
	export.volumeId = cmdExport.Flag.Int("volumeId", -1, "a volume id. The volume .dat and .idx files should already exist in the dir.")
}

var (
	output      = cmdExport.Flag.String("o", "", "output tar file name, must ends with .tar, or just a \"-\" for stdout")
	format      = cmdExport.Flag.String("fileNameFormat", defaultFnFormat, "filename formatted with {{.Id}} {{.Name}} {{.Ext}}")
	newer       = cmdExport.Flag.String("newer", "", "export only files newer than this time, default is all files. Must be specified in RFC3339 without timezone, e.g. 2006-01-02T15:04:05")
	showDeleted = cmdExport.Flag.Bool("deleted", false, "export deleted files. only applies if -o is not specified")
	limit       = cmdExport.Flag.Int("limit", 0, "only show first n entries if specified")
	exportFid   = cmdExport.Flag.String("fid", "", "export only this file id, e.g. 234,01637037d6. The output -o is the file content, or \"-\" for stdout, unless it ends with .tar. Default is the file id with the file's extension")

	tarOutputFile          *tar.Writer
	tarHeader              tar.Header
	fileNameTemplate       *template.Template
	fileNameTemplateBuffer = bytes.NewBuffer(nil)
	newerThan              time.Time
	newerThanUnix          int64 = -1
	localLocation, _             = time.LoadLocation("Local")
)

func printNeedle(vid needle.VolumeId, n *needle.Needle, version needle.Version, deleted bool, offset int64, onDiskSize int64) {
	key := needle.NewFileIdFromNeedle(vid, n).String()
	size := int32(n.DataSize)
	if version == needle.Version1 {
		size = int32(n.Size)
	}
	fmt.Printf("%s\t%s\t%d\t%t\t%s\t%s\t%s\t%t\t%d\t%d\n",
		key,
		n.Name,
		size,
		n.IsCompressed(),
		n.Mime,
		n.LastModifiedString(),
		n.Ttl.String(),
		deleted,
		offset,
		offset+onDiskSize,
	)
}

type VolumeFileScanner4Export struct {
	version   needle.Version
	counter   int
	needleMap *needle_map.MemDb
	vid       needle.VolumeId
}

func (scanner *VolumeFileScanner4Export) VisitSuperBlock(superBlock super_block.SuperBlock) error {
	scanner.version = superBlock.Version
	return nil

}
func (scanner *VolumeFileScanner4Export) ReadNeedleBody() bool {
	return true
}

func (scanner *VolumeFileScanner4Export) VisitNeedle(n *needle.Needle, offset int64, needleHeader, needleBody []byte) error {
	needleMap := scanner.needleMap
	vid := scanner.vid

	nv, ok := needleMap.Get(n.Id)
	glog.V(3).Infof("key %d offset %d size %d disk_size %d compressed %v ok %v nv %+v",
		n.Id, offset, n.Size, n.DiskSize(scanner.version), n.IsCompressed(), ok, nv)
	if *showDeleted && n.Size > 0 || ok && nv.Size.IsValid() && nv.Offset.ToActualOffset() == offset {
		if newerThanUnix >= 0 && n.HasLastModifiedDate() && n.LastModified < uint64(newerThanUnix) {
			glog.V(3).Infof("Skipping this file, as it's old enough: LastModified %d vs %d",
				n.LastModified, newerThanUnix)
			return nil
		}
		scanner.counter++
		if *limit > 0 && scanner.counter > *limit {
			return io.EOF
		}
		if tarOutputFile != nil {
			return writeFile(vid, n)
		} else {
			printNeedle(vid, n, scanner.version, false, offset, n.DiskSize(scanner.version))
			return nil
		}
	}
	if !ok {
		if *showDeleted && tarOutputFile == nil {
			if n.DataSize > 0 {
				printNeedle(vid, n, scanner.version, true, offset, n.DiskSize(scanner.version))
			} else {
				n.Name = []byte("*tombstone")
				printNeedle(vid, n, scanner.version, true, offset, n.DiskSize(scanner.version))
			}
		}
		glog.V(2).Infof("This seems deleted %d size %d", n.Id, n.Size)
	} else {
		glog.V(2).Infof("Skipping later-updated Id %d size %d", n.Id, n.Size)
	}
	return nil
}

func runExport(cmd *Command, args []string) bool {

	*export.dir = util.ResolvePath(*export.dir)
	*output = util.ResolvePath(*output)

	var err error

	if *newer != "" {
		if newerThan, err = time.ParseInLocation(timeFormat, *newer, localLocation); err != nil {
			fmt.Println("cannot parse 'newer' argument: " + err.Error())
			return false
		}
		newerThanUnix = newerThan.Unix()
	}

	var fileId *needle.FileId
	if *exportFid != "" {
		if fileId, err = needle.ParseFileIdFromString(*exportFid); err != nil {
			fmt.Println("cannot parse 'fid' argument: " + err.Error())
			return false
		}
		if *export.volumeId == -1 {
			*export.volumeId = int(fileId.VolumeId)
		} else if *export.volumeId != int(fileId.VolumeId) {
			fmt.Printf("fid %s is not in volume %d\n", *exportFid, *export.volumeId)
			return false
		}
	}

	if *export.volumeId == -1 {
		return false
	}

	// One file is written as its content, unless a tar file is asked for.
	if *output != "" && (fileId == nil || strings.HasSuffix(*output, ".tar")) {
		if *output != "-" && !strings.HasSuffix(*output, ".tar") {
			fmt.Println("the output file", *output, "should be '-' or end with .tar")
			return false
		}

		if fileNameTemplate, err = template.New("name").Parse(*format); err != nil {
			fmt.Println("cannot parse format " + *format + ": " + err.Error())
			return false
		}

		var outputFile *os.File
		if *output == "-" {
			outputFile = os.Stdout
		} else {
			if outputFile, err = os.Create(*output); err != nil {
				glog.Fatalf("cannot open output tar %s: %s", *output, err)
			}
		}
		defer outputFile.Close()
		tarOutputFile = tar.NewWriter(outputFile)
		defer tarOutputFile.Close()
		t := time.Now()
		tarHeader = tar.Header{Mode: 0644,
			ModTime: t, Uid: os.Getuid(), Gid: os.Getgid(),
			Typeflag:   tar.TypeReg,
			AccessTime: t, ChangeTime: t}
	}

	fileName := strconv.Itoa(*export.volumeId)
	if *export.collection != "" {
		fileName = *export.collection + "_" + fileName
	}
	vid := needle.VolumeId(*export.volumeId)

	needleMap := needle_map.NewMemDb()
	defer needleMap.Close()

	// LoadFromIdx takes an index file it cannot open for an empty one, which
	// lists or exports nothing.
	idxFileName := path.Join(*export.dir, fileName+".idx")
	if _, err := os.Stat(idxFileName); err != nil {
		glog.Fatalf("cannot load needle map from %s.idx: %s. Run \"weed fix\" to recreate a missing index file", fileName, err)
	}
	if err := needleMap.LoadFromIdx(idxFileName); err != nil {
		glog.Fatalf("cannot load needle map from %s.idx: %s", fileName, err)
	}

	if fileId != nil {
		if err := exportOneFile(fileId, needleMap); err != nil {
			fmt.Fprintf(os.Stderr, "cannot export %s: %s\n", fileId, err)
			SetCommandExitStatus(1)
		}
		return true
	}

	volumeFileScanner := &VolumeFileScanner4Export{
		needleMap: needleMap,
		vid:       vid,
	}

	if tarOutputFile == nil {
		fmt.Print(listHeader)
	}

	err = storage.ScanVolumeFile(*export.dir, *export.collection, vid, storage.NeedleMapInMemory, volumeFileScanner)
	if err != nil && err != io.EOF {
		glog.Errorf("Export Volume File [ERROR] %s\n", err)
	}
	return true
}

// exportOneFile exports the file its index entry points at, without scanning
// the volume file.
func exportOneFile(fileId *needle.FileId, needleMap *needle_map.MemDb) error {
	nv, ok := needleMap.Get(fileId.Key)
	if !ok || !nv.Size.IsValid() {
		return fmt.Errorf("not in the index, or deleted")
	}
	offset := nv.Offset.ToActualOffset()
	n, err := storage.ReadVolumeFileNeedle(*export.dir, *export.collection, fileId.VolumeId, storage.NeedleMapInMemory, offset, nv.Size)
	if err != nil {
		return err
	}
	if n.Id != fileId.Key || n.Cookie != fileId.Cookie {
		return fmt.Errorf("found %s at offset %d", needle.NewFileIdFromNeedle(fileId.VolumeId, n), offset)
	}

	if tarOutputFile != nil {
		return writeFile(fileId.VolumeId, n)
	}

	data := n.Data
	if n.IsCompressed() && util.IsGzippedContent(data) {
		if data, err = util.DecompressData(data); err != nil {
			return err
		}
	}
	if *output == "-" {
		_, err = os.Stdout.Write(data)
		return err
	}
	fileName := *output
	if fileName == "" {
		fileName = fileId.String() + needleExt(n)
	}
	if err = os.WriteFile(fileName, data, 0644); err != nil {
		return err
	}
	// As in a tar file, the file is as old as the one that was stored.
	if n.HasLastModifiedDate() {
		modTime := time.Unix(int64(n.LastModified), 0)
		if err = os.Chtimes(fileName, modTime, modTime); err != nil {
			return err
		}
	}
	if *output == "" {
		fmt.Println(fileName)
	}
	return nil
}

// needleExt is the extension of the name the file was stored under, or else
// one for its mime type.
func needleExt(n *needle.Needle) string {
	if ext := filepath.Ext(string(n.Name)); ext != "" {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(string(n.Mime)); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

type nameParams struct {
	Name string
	Id   types.NeedleId
	Mime string
	Key  string
	Ext  string
}

func writeFile(vid needle.VolumeId, n *needle.Needle) (err error) {
	key := needle.NewFileIdFromNeedle(vid, n).String()
	fileNameTemplateBuffer.Reset()
	if err = fileNameTemplate.Execute(fileNameTemplateBuffer,
		nameParams{
			Name: string(n.Name),
			Id:   n.Id,
			Mime: string(n.Mime),
			Key:  key,
			Ext:  filepath.Ext(string(n.Name)),
		},
	); err != nil {
		return err
	}

	fileName := fileNameTemplateBuffer.String()

	if n.IsCompressed() {
		if util.IsGzippedContent(n.Data) && path.Ext(fileName) != ".gz" {
			fileName = fileName + ".gz"
		}
		// TODO other compression method
	}

	tarHeader.Name, tarHeader.Size = fileName, int64(len(n.Data))
	if n.HasLastModifiedDate() {
		tarHeader.ModTime = time.Unix(int64(n.LastModified), 0)
	} else {
		tarHeader.ModTime = time.Unix(0, 0)
	}
	tarHeader.ChangeTime = tarHeader.ModTime
	if err = tarOutputFile.WriteHeader(&tarHeader); err != nil {
		return err
	}
	_, err = tarOutputFile.Write(n.Data)
	return
}
