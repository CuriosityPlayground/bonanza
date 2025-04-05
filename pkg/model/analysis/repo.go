package analysis

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/diff"
	"github.com/buildbarn/bonanza/pkg/evaluation"
	"github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	"github.com/buildbarn/bonanza/pkg/model/core/btree"
	model_encoding "github.com/buildbarn/bonanza/pkg/model/encoding"
	model_filesystem "github.com/buildbarn/bonanza/pkg/model/filesystem"
	model_parser "github.com/buildbarn/bonanza/pkg/model/parser"
	model_starlark "github.com/buildbarn/bonanza/pkg/model/starlark"
	model_analysis_pb "github.com/buildbarn/bonanza/pkg/proto/model/analysis"
	model_command_pb "github.com/buildbarn/bonanza/pkg/proto/model/command"
	model_filesystem_pb "github.com/buildbarn/bonanza/pkg/proto/model/filesystem"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"
	"github.com/buildbarn/bonanza/pkg/search"
	"github.com/buildbarn/bonanza/pkg/starlark/unpack"
	"github.com/buildbarn/bonanza/pkg/storage/dag"
	"github.com/buildbarn/bonanza/pkg/storage/object"
	"github.com/kballard/go-shellquote"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type jsonOrderedMapEntry[T any] struct {
	key   string
	value T
}

// jsonOrderedMap can be used instead of map[string]T to unmarshal JSON
// objects where the original order of fields is preserved.
//
// This is necessary to unmarshal patches declared in source.json files
// that are served by Bazel Central Registry (BCR), as those need to be
// applied in the same order as they are listed in the JSON object.
//
// More details: https://github.com/bazelbuild/bazel/issues/25369
type jsonOrderedMap[T any] []jsonOrderedMapEntry[T]

func (m *jsonOrderedMap[T]) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("expected start of ordered map")
	}
	for {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		if keyToken == json.Delim('}') {
			return nil
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("unexpected token %s", keyToken)
		}

		var value T
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		*m = append(*m, jsonOrderedMapEntry[T]{
			key:   key,
			value: value,
		})
	}
}

// sourceJSON corresponds to the format of source.json files that are
// served by Bazel Central Registry (BCR).
type sourceJSON struct {
	Integrity   string                 `json:"integrity"`
	PatchStrip  int                    `json:"patch_strip"`
	Patches     jsonOrderedMap[string] `json:"patches"`
	StripPrefix string                 `json:"strip_prefix"`
	URL         string                 `json:"url"`
}

type changeTrackingDirectory[TReference object.BasicReference] struct {
	// If set, the directory has not been accessed, and its contents
	// are still identical to the original version. If not set, the
	// directory has been accessed and potentially modified,
	// requiring it to be recomputed and uploaded once again.
	currentReference model_core.Message[*model_filesystem_pb.DirectoryReference, TReference]

	directories map[path.Component]*changeTrackingDirectory[TReference]
	files       map[path.Component]*changeTrackingFile[TReference]
	symlinks    map[path.Component]string
}

func (d *changeTrackingDirectory[TReference]) setContents(contents model_core.Message[*model_filesystem_pb.Directory, TReference], options *changeTrackingDirectoryLoadOptions[TReference]) error {
	leaves, err := model_filesystem.DirectoryGetLeaves(options.context, options.leavesReader, contents)
	if err != nil {
		return err
	}

	d.files = make(map[path.Component]*changeTrackingFile[TReference], len(leaves.Message.Files))
	for _, file := range leaves.Message.Files {
		name, ok := path.NewComponent(file.Name)
		if !ok {
			return fmt.Errorf("file %#v has an invalid name", file.Name)
		}
		properties := file.Properties
		if properties == nil {
			return fmt.Errorf("file %#v has no properties", file.Name)
		}
		d.files[name] = &changeTrackingFile[TReference]{
			isExecutable: properties.IsExecutable,
			contents: unmodifiedFileContents[TReference]{
				contents: model_core.Nested(leaves, properties.Contents),
			},
		}
	}
	d.symlinks = make(map[path.Component]string, len(leaves.Message.Symlinks))
	for _, symlink := range leaves.Message.Symlinks {
		name, ok := path.NewComponent(symlink.Name)
		if !ok {
			return fmt.Errorf("symbolic link %#v has an invalid name", symlink.Name)
		}
		d.symlinks[name] = symlink.Target
	}

	d.directories = make(map[path.Component]*changeTrackingDirectory[TReference], len(contents.Message.Directories))
	for _, directory := range contents.Message.Directories {
		name, ok := path.NewComponent(directory.Name)
		if !ok {
			return fmt.Errorf("directory %#v has an invalid name", directory.Name)
		}
		switch childContents := directory.Contents.(type) {
		case *model_filesystem_pb.DirectoryNode_ContentsExternal:
			d.directories[name] = &changeTrackingDirectory[TReference]{
				currentReference: model_core.Nested(contents, childContents.ContentsExternal),
			}
		case *model_filesystem_pb.DirectoryNode_ContentsInline:
			dChild := &changeTrackingDirectory[TReference]{}
			if err := dChild.setContents(
				model_core.Nested(contents, childContents.ContentsInline),
				options,
			); err != nil {
				return err
			}
			d.directories[name] = dChild
		default:
			return errors.New("unknown directory contents type")
		}
	}
	return nil
}

func (d *changeTrackingDirectory[TReference]) getOrCreateDirectory(name path.Component) (*changeTrackingDirectory[TReference], error) {
	dChild, ok := d.directories[name]
	if !ok {
		if _, ok := d.files[name]; ok {
			return nil, errors.New("a file with this name already exists")
		}
		if _, ok := d.symlinks[name]; ok {
			return nil, errors.New("a symbolic link with this name already exists")
		}
		if d.directories == nil {
			d.directories = map[path.Component]*changeTrackingDirectory[TReference]{}
		}
		dChild = &changeTrackingDirectory[TReference]{}
		d.directories[name] = dChild
	}
	return dChild, nil
}

func (d *changeTrackingDirectory[TReference]) setFile(loadOptions *changeTrackingDirectoryLoadOptions[TReference], name path.Component, f *changeTrackingFile[TReference]) error {
	if err := d.maybeLoadContents(loadOptions); err != nil {
		return err
	}

	if d.files == nil {
		d.files = map[path.Component]*changeTrackingFile[TReference]{}
	}
	d.files[name] = f
	delete(d.directories, name)
	delete(d.symlinks, name)

	return nil
}

type changeTrackingDirectoryLoadOptions[TReference any] struct {
	context         context.Context
	directoryReader model_parser.ParsedObjectReader[TReference, model_core.Message[*model_filesystem_pb.Directory, TReference]]
	leavesReader    model_parser.ParsedObjectReader[TReference, model_core.Message[*model_filesystem_pb.Leaves, TReference]]
}

func (d *changeTrackingDirectory[TReference]) maybeLoadContents(options *changeTrackingDirectoryLoadOptions[TReference]) error {
	if reference := d.currentReference; reference.IsSet() {
		// Directory has not been accessed before. Load it from
		// storage and ingest its contents.
		directoryMessage, err := model_parser.Dereference(options.context, options.directoryReader, model_core.Nested(reference, reference.Message.GetReference()))
		if err != nil {
			return err
		}
		d.currentReference.Clear()
		if err := d.setContents(directoryMessage, options); err != nil {
			return err
		}
	}
	return nil
}

type changeTrackingFile[TReference object.BasicReference] struct {
	isExecutable bool
	contents     changeTrackingFileContents[TReference]
}

type changeTrackingFileContents[TReference object.BasicReference] interface {
	createFileMerkleTree(ctx context.Context, options *capturableChangeTrackingDirectoryOptions[TReference]) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, model_core.FileBackedObjectLocation], error)
	openRead(ctx context.Context, fileReader *model_filesystem.FileReader[TReference], patchedFiles io.ReaderAt) (io.Reader, error)
}

type unmodifiedFileContents[TReference object.BasicReference] struct {
	contents model_core.Message[*model_filesystem_pb.FileContents, TReference]
}

func (fc unmodifiedFileContents[TReference]) createFileMerkleTree(ctx context.Context, options *capturableChangeTrackingDirectoryOptions[TReference]) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, model_core.FileBackedObjectLocation], error) {
	// TODO: Stop using ExistingFileBackedObjectLocation, as it's not
	// correct in case the file was being written to storage
	// asynchronously.
	return model_core.NewPatchedMessageFromExisting(
		fc.contents,
		func(index int) model_core.FileBackedObjectLocation {
			return model_core.ExistingFileBackedObjectLocation
		},
	), nil
}

func (fc unmodifiedFileContents[TReference]) openRead(ctx context.Context, fileReader *model_filesystem.FileReader[TReference], patchedFiles io.ReaderAt) (io.Reader, error) {
	entry, err := model_filesystem.NewFileContentsEntryFromProto(fc.contents)
	if err != nil {
		return nil, err
	}
	return fileReader.FileOpenRead(ctx, entry, 0), nil
}

type patchedFileContents[TReference object.BasicReference] struct {
	offsetBytes int64
	sizeBytes   int64
}

func (fc patchedFileContents[TReference]) createFileMerkleTree(ctx context.Context, options *capturableChangeTrackingDirectoryOptions[TReference]) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, model_core.FileBackedObjectLocation], error) {
	return model_filesystem.CreateFileMerkleTree(
		ctx,
		options.fileCreationParameters,
		io.NewSectionReader(options.patchedFiles, fc.offsetBytes, fc.sizeBytes),
		options.fileMerkleTreeCapturer,
	)
}

func (fc patchedFileContents[TReference]) openRead(ctx context.Context, fileReader *model_filesystem.FileReader[TReference], patchedFiles io.ReaderAt) (io.Reader, error) {
	return io.NewSectionReader(patchedFiles, fc.offsetBytes, fc.sizeBytes), nil
}

type changeTrackingDirectoryResolver[TReference object.BasicReference] struct {
	loadOptions      *changeTrackingDirectoryLoadOptions[TReference]
	currentDirectory *changeTrackingDirectory[TReference]
}

func (r *changeTrackingDirectoryResolver[TReference]) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := r.currentDirectory
	if err := d.maybeLoadContents(r.loadOptions); err != nil {
		return nil, err
	}

	if dChild, ok := d.directories[name]; ok {
		r.currentDirectory = dChild
		return path.GotDirectory{
			Child:        r,
			IsReversible: true,
		}, nil
	}

	return nil, fmt.Errorf("directory %#v does not exist", name.String())
}

func (r *changeTrackingDirectoryResolver[TReference]) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	return path.OnTerminalViaOnDirectory(r, name)
}

func (r *changeTrackingDirectoryResolver[TReference]) OnUp() (path.ComponentWalker, error) {
	return nil, errors.New("path cannot go up")
}

type capturableChangeTrackingDirectoryOptions[TReference any] struct {
	context                context.Context
	directoryReader        model_parser.ParsedObjectReader[TReference, model_core.Message[*model_filesystem_pb.Directory, TReference]]
	fileCreationParameters *model_filesystem.FileCreationParameters
	fileMerkleTreeCapturer model_filesystem.FileMerkleTreeCapturer[model_core.FileBackedObjectLocation]
	patchedFiles           io.ReaderAt
}

type capturableChangeTrackingDirectory[TReference object.BasicReference] struct {
	options   *capturableChangeTrackingDirectoryOptions[TReference]
	directory *changeTrackingDirectory[TReference]
}

func (cd *capturableChangeTrackingDirectory[TReference]) Close() error {
	return nil
}

func (cd *capturableChangeTrackingDirectory[TReference]) EnterCapturableDirectory(name path.Component) (*model_filesystem.CreatedDirectory[model_core.FileBackedObjectLocation], model_filesystem.CapturableDirectory[model_core.FileBackedObjectLocation, model_core.FileBackedObjectLocation], error) {
	dChild, ok := cd.directory.directories[name]
	if !ok {
		panic("attempted to enter non-existent directory")
	}
	if reference := dChild.currentReference; reference.IsSet() {
		// Directory has not been modified. Load the copy from
		// storage, so that it may potentially be inlined into
		// the parent directory.
		directoryMessage, err := model_parser.Dereference(cd.options.context, cd.options.directoryReader, model_core.Nested(reference, reference.Message.GetReference()))
		if err != nil {
			return nil, nil, err
		}
		// TODO: Stop using ExistingFileBackedObjectLocation, as it's
		// not correct in case the directory was being written to
		// storage asynchronously.
		return &model_filesystem.CreatedDirectory[model_core.FileBackedObjectLocation]{
			Message: model_core.NewPatchedMessageFromExisting(
				directoryMessage,
				func(index int) model_core.FileBackedObjectLocation {
					return model_core.ExistingFileBackedObjectLocation
				},
			),
			MaximumSymlinkEscapementLevels: dChild.currentReference.Message.GetMaximumSymlinkEscapementLevels(),
		}, nil, nil
	}

	// Directory contains one or more changes. Recurse into it.
	return nil,
		&capturableChangeTrackingDirectory[TReference]{
			options:   cd.options,
			directory: dChild,
		},
		nil
}

func (cd *capturableChangeTrackingDirectory[TReference]) OpenForFileMerkleTreeCreation(name path.Component) (model_filesystem.CapturableFile[model_core.FileBackedObjectLocation], error) {
	file, ok := cd.directory.files[name]
	if !ok {
		panic("attempted to enter non-existent file")
	}
	return &capturableChangeTrackingFile[TReference]{
		options:  cd.options,
		contents: file.contents,
	}, nil
}

func (cd *capturableChangeTrackingDirectory[TReference]) ReadDir() ([]filesystem.FileInfo, error) {
	d := cd.directory
	infos := make(filesystem.FileInfoList, 0, len(d.directories)+len(d.files)+len(d.symlinks))
	for name := range d.directories {
		infos = append(infos, filesystem.NewFileInfo(name, filesystem.FileTypeDirectory, false))
	}
	for name, file := range d.files {
		infos = append(infos, filesystem.NewFileInfo(name, filesystem.FileTypeRegularFile, file.isExecutable))
	}
	for name := range d.symlinks {
		infos = append(infos, filesystem.NewFileInfo(name, filesystem.FileTypeSymlink, false))
	}
	sort.Sort(infos)
	return infos, nil
}

func (cd *capturableChangeTrackingDirectory[TReference]) Readlink(name path.Component) (path.Parser, error) {
	target, ok := cd.directory.symlinks[name]
	if !ok {
		panic("attempted to read non-existent symbolic link")
	}
	return path.UNIXFormat.NewParser(target), nil
}

type capturableChangeTrackingFile[TReference object.BasicReference] struct {
	options  *capturableChangeTrackingDirectoryOptions[TReference]
	contents changeTrackingFileContents[TReference]
}

func (cf *capturableChangeTrackingFile[TReference]) CreateFileMerkleTree(ctx context.Context) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, model_core.FileBackedObjectLocation], error) {
	return cf.contents.createFileMerkleTree(ctx, cf.options)
}

func (cf *capturableChangeTrackingFile[TReference]) Discard() {}

type strippingComponentWalker struct {
	remainder            path.ComponentWalker
	additionalStripCount int
}

func newStrippingComponentWalker(remainder path.ComponentWalker, stripCount int) path.ComponentWalker {
	return strippingComponentWalker{
		remainder:            remainder,
		additionalStripCount: stripCount,
	}.stripComponent()
}

func (cw strippingComponentWalker) stripComponent() path.ComponentWalker {
	if cw.additionalStripCount > 0 {
		return strippingComponentWalker{
			remainder:            cw.remainder,
			additionalStripCount: cw.additionalStripCount - 1,
		}
	}
	return cw.remainder
}

func (cw strippingComponentWalker) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	return path.GotDirectory{
		Child:        cw.stripComponent(),
		IsReversible: false,
	}, nil
}

func (cw strippingComponentWalker) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	return nil, nil
}

func (cw strippingComponentWalker) OnUp() (path.ComponentWalker, error) {
	return cw.stripComponent(), nil
}

type changeTrackingDirectoryExistingFileResolver[TReference object.BasicReference] struct {
	path.TerminalNameTrackingComponentWalker

	loadOptions *changeTrackingDirectoryLoadOptions[TReference]
	stack       util.NonEmptyStack[*changeTrackingDirectory[TReference]]
	gotScope    bool
}

func (r *changeTrackingDirectoryExistingFileResolver[TReference]) OnAbsolute() (path.ComponentWalker, error) {
	r.stack.PopAll()
	r.gotScope = true
	return r, nil
}

func (r *changeTrackingDirectoryExistingFileResolver[TReference]) OnRelative() (path.ComponentWalker, error) {
	r.gotScope = true
	return r, nil
}

func (r *changeTrackingDirectoryExistingFileResolver[TReference]) OnDriveLetter(driveLetter rune) (path.ComponentWalker, error) {
	return nil, errors.New("drive letters are not supported")
}

var errDirectoryDoesNotExist = errors.New("directory does not exist")

func (r *changeTrackingDirectoryExistingFileResolver[TReference]) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := r.stack.Peek()
	if err := d.maybeLoadContents(r.loadOptions); err != nil {
		return nil, err
	}
	if dChild, ok := d.directories[name]; ok {
		r.stack.Push(dChild)
		return path.GotDirectory{
			Child:        r,
			IsReversible: true,
		}, nil
	}
	return nil, errDirectoryDoesNotExist
}

func (r *changeTrackingDirectoryExistingFileResolver[TReference]) OnUp() (path.ComponentWalker, error) {
	if _, ok := r.stack.PopSingle(); !ok {
		return nil, errors.New("path resolves to a location above the root directory")
	}
	return r, nil
}

func (r *changeTrackingDirectoryExistingFileResolver[TReference]) getFile() (*changeTrackingFile[TReference], error) {
	if r.TerminalName == nil {
		return nil, errors.New("path does not resolve to a file")
	}
	d := r.stack.Peek()
	if err := d.maybeLoadContents(r.loadOptions); err != nil {
		return nil, err
	}
	if f, ok := d.files[*r.TerminalName]; ok {
		return f, nil
	}
	return nil, errors.New("file does not exist")
}

type changeTrackingDirectoryNewDirectoryResolver[TReference object.BasicReference] struct {
	loadOptions *changeTrackingDirectoryLoadOptions[TReference]
	stack       util.NonEmptyStack[*changeTrackingDirectory[TReference]]
}

func (r *changeTrackingDirectoryNewDirectoryResolver[TReference]) OnAbsolute() (path.ComponentWalker, error) {
	r.stack.PopAll()
	return r, nil
}

func (r *changeTrackingDirectoryNewDirectoryResolver[TReference]) OnRelative() (path.ComponentWalker, error) {
	return r, nil
}

func (r *changeTrackingDirectoryNewDirectoryResolver[TReference]) OnDriveLetter(driveLetter rune) (path.ComponentWalker, error) {
	return nil, errors.New("drive letters are not supported")
}

func (r *changeTrackingDirectoryNewDirectoryResolver[TReference]) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := r.stack.Peek()
	if err := d.maybeLoadContents(r.loadOptions); err != nil {
		return nil, err
	}

	dChild, err := d.getOrCreateDirectory(name)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory %#v: %w", name.String(), err)
	}

	r.stack.Push(dChild)
	return path.GotDirectory{
		Child:        r,
		IsReversible: true,
	}, nil
}

func (r *changeTrackingDirectoryNewDirectoryResolver[TReference]) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	return path.OnTerminalViaOnDirectory(r, name)
}

func (r *changeTrackingDirectoryNewDirectoryResolver[TReference]) OnUp() (path.ComponentWalker, error) {
	if _, ok := r.stack.PopSingle(); !ok {
		return nil, errors.New("path resolves to a location above the root directory")
	}
	return r, nil
}

type changeTrackingDirectoryNewFileResolver[TReference object.BasicReference] struct {
	path.TerminalNameTrackingComponentWalker

	loadOptions *changeTrackingDirectoryLoadOptions[TReference]
	stack       util.NonEmptyStack[*changeTrackingDirectory[TReference]]
}

func (r *changeTrackingDirectoryNewFileResolver[TReference]) OnAbsolute() (path.ComponentWalker, error) {
	r.stack.PopAll()
	return r, nil
}

func (r *changeTrackingDirectoryNewFileResolver[TReference]) OnRelative() (path.ComponentWalker, error) {
	return r, nil
}

func (r *changeTrackingDirectoryNewFileResolver[TReference]) OnDriveLetter(driveLetter rune) (path.ComponentWalker, error) {
	return nil, errors.New("drive letters are not supported")
}

func (r *changeTrackingDirectoryNewFileResolver[TReference]) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := r.stack.Peek()
	if err := d.maybeLoadContents(r.loadOptions); err != nil {
		return nil, err
	}

	dChild, err := d.getOrCreateDirectory(name)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory %#v: %w", name.String(), err)
	}

	r.stack.Push(dChild)
	return path.GotDirectory{
		Child:        r,
		IsReversible: true,
	}, nil
}

func (r *changeTrackingDirectoryNewFileResolver[TReference]) OnUp() (path.ComponentWalker, error) {
	if _, ok := r.stack.PopSingle(); !ok {
		return nil, errors.New("path resolves to a location above the root directory")
	}
	return r, nil
}

func inferArchiveFormatFromURL(url string) (model_analysis_pb.HttpArchiveContents_Key_Format, bool) {
	if strings.HasSuffix(url, ".tar.gz") {
		return model_analysis_pb.HttpArchiveContents_Key_TAR_GZ, true
	}
	if strings.HasSuffix(url, ".tar.xz") {
		return model_analysis_pb.HttpArchiveContents_Key_TAR_XZ, true
	}
	if strings.HasSuffix(url, ".zip") {
		return model_analysis_pb.HttpArchiveContents_Key_ZIP, true
	}
	return 0, false
}

func parseSubresourceIntegrity(integrity string) (*model_analysis_pb.SubresourceIntegrity, error) {
	dash := strings.IndexByte(integrity, '-')
	if dash < 0 {
		return nil, errors.New("subresource integrity does not contain a dash")
	}

	hashAlgorithmStr := integrity[:dash]
	hashAlgorithm, ok := model_analysis_pb.SubresourceIntegrity_HashAlgorithm_value[strings.ToUpper(hashAlgorithmStr)]
	if !ok {
		return nil, fmt.Errorf("unknown hash algorithm %#v", hashAlgorithmStr)
	}

	hashStr := integrity[dash+1:]
	hash, err := base64.StdEncoding.DecodeString(hashStr)
	if err != nil {
		return nil, fmt.Errorf("invalid hash %#v: %w", hashStr, err)
	}

	return &model_analysis_pb.SubresourceIntegrity{
		HashAlgorithm: model_analysis_pb.SubresourceIntegrity_HashAlgorithm(hashAlgorithm),
		Hash:          hash,
	}, nil
}

func parseSubresourceIntegrityOrSHA256(integrity, sha256 string) (*model_analysis_pb.SubresourceIntegrity, error) {
	if integrity != "" {
		return parseSubresourceIntegrity(integrity)
	}
	if sha256 != "" {
		sha256Bytes, err := hex.DecodeString(sha256)
		if err != nil {
			return nil, fmt.Errorf("invalid sha256: %w", err)
		}
		return &model_analysis_pb.SubresourceIntegrity{
			HashAlgorithm: model_analysis_pb.SubresourceIntegrity_SHA256,
			Hash:          sha256Bytes,
		}, nil
	}
	return nil, nil
}

func (c *baseComputer[TReference, TMetadata]) fetchModuleFromRegistry(
	ctx context.Context,
	module *model_analysis_pb.BuildListModule,
	e RepoEnvironment[TReference, TMetadata],
	singleVersionOverridePatchLabels []string,
	singleVersionOverridePatchCommands []string,
	singleVersionOverridePatchStrip int,
) (PatchedRepoValue, error) {
	fileReader, gotFileReader := e.GetFileReaderValue(&model_analysis_pb.FileReader_Key{})
	if !gotFileReader {
		return PatchedRepoValue{}, evaluation.ErrMissingDependency
	}

	sourceJSONURL, err := url.JoinPath(
		module.RegistryUrl,
		"modules",
		module.Name,
		module.Version,
		"source.json",
	)
	if err != nil {
		return PatchedRepoValue{}, fmt.Errorf("failed to construct URL for module %s with version %s in registry %#v: %w", module.Name, module.Version, module.RegistryUrl, err)
	}

	sourceJSONContentsValue := e.GetHttpFileContentsValue(
		&model_analysis_pb.HttpFileContents_Key{
			FetchOptions: &model_analysis_pb.HttpFetchOptions{
				Urls: []string{sourceJSONURL},
			},
		},
	)
	if !sourceJSONContentsValue.IsSet() {
		return PatchedRepoValue{}, evaluation.ErrMissingDependency
	}
	if sourceJSONContentsValue.Message.Exists == nil {
		return PatchedRepoValue{}, fmt.Errorf("file at URL %#v does not exist", sourceJSONURL)
	}
	sourceJSONContentsEntry, err := model_filesystem.NewFileContentsEntryFromProto(
		model_core.Nested(sourceJSONContentsValue, sourceJSONContentsValue.Message.Exists.Contents),
	)
	if err != nil {
		return PatchedRepoValue{}, fmt.Errorf("invalid file contents: %w", err)
	}

	sourceJSONData, err := fileReader.FileReadAll(ctx, sourceJSONContentsEntry, 1<<20)
	if err != nil {
		return PatchedRepoValue{}, err
	}
	var sourceJSON sourceJSON
	if err := json.Unmarshal(sourceJSONData, &sourceJSON); err != nil {
		return PatchedRepoValue{}, fmt.Errorf("invalid JSON contents for %#v: %w", sourceJSONURL, err)
	}

	archiveFormat, ok := inferArchiveFormatFromURL(sourceJSON.URL)
	if !ok {
		return PatchedRepoValue{}, fmt.Errorf("cannot derive archive format from file extension of URL %#v", sourceJSONURL)
	}

	integrity, err := parseSubresourceIntegrity(sourceJSON.Integrity)
	if err != nil {
		return PatchedRepoValue{}, fmt.Errorf("invalid subresource integrity %#v in %#v: %w", sourceJSON.Integrity, sourceJSONURL, err)
	}

	// Download source archive and all patches that need to be
	// applied. Return all the errors at once - this way, anything
	// that needs downloading is done.
	missingDependencies := false
	archiveContentsValue := e.GetHttpArchiveContentsValue(&model_analysis_pb.HttpArchiveContents_Key{
		FetchOptions: &model_analysis_pb.HttpFetchOptions{
			Urls:      []string{sourceJSON.URL},
			Integrity: integrity,
		},
		Format: archiveFormat,
	})
	if !archiveContentsValue.IsSet() {
		missingDependencies = true
	}

	// Process patches stored in source.json.
	patchesToApply := make([]patchToApply[TReference], 0, len(sourceJSON.Patches)+len(singleVersionOverridePatchLabels))
	for _, patchEntry := range sourceJSON.Patches {
		patchURL, err := url.JoinPath(
			module.RegistryUrl,
			"modules",
			module.Name,
			module.Version,
			"patches",
			patchEntry.key,
		)
		if err != nil {
			return PatchedRepoValue{}, fmt.Errorf("failed to construct URL for patch %s of module %s with version %s in registry %#v: %w", patchEntry.key, module.Name, module.Version, module.RegistryUrl, err)
		}

		integrity, err := parseSubresourceIntegrity(patchEntry.value)
		if err != nil {
			return PatchedRepoValue{}, fmt.Errorf("invalid subresource integrity %#v for patch %#v: %w", patchEntry.value, patchURL, err)
		}

		patchContentsValue := e.GetHttpFileContentsValue(&model_analysis_pb.HttpFileContents_Key{
			FetchOptions: &model_analysis_pb.HttpFetchOptions{
				Urls:      []string{patchURL},
				Integrity: integrity,
			},
		})
		if !patchContentsValue.IsSet() {
			missingDependencies = true
			continue
		}
		if patchContentsValue.Message.Exists == nil {
			return PatchedRepoValue{}, fmt.Errorf("patch at URL %#v does not exist", patchEntry.key)
		}

		patchContentsEntry, err := model_filesystem.NewFileContentsEntryFromProto(
			model_core.Nested(patchContentsValue, patchContentsValue.Message.Exists.Contents),
		)
		if err != nil {
			return PatchedRepoValue{}, fmt.Errorf("invalid file contents for patch %#v: %w", patchEntry.key, err)
		}

		patchesToApply = append(patchesToApply, patchToApply[TReference]{
			filename:          patchEntry.key,
			strip:             sourceJSON.PatchStrip,
			fileContentsEntry: patchContentsEntry,
		})
	}

	// If a single_version_override() is present, we may need to
	// apply additional patches.
	for _, patchLabelStr := range singleVersionOverridePatchLabels {
		patchLabel, err := label.NewCanonicalLabel(patchLabelStr)
		if err != nil {
			return PatchedRepoValue{}, fmt.Errorf("invalid single version override patch label %#v: %w", patchLabelStr)
		}
		patchPropertiesValue := e.GetFilePropertiesValue(&model_analysis_pb.FileProperties_Key{
			CanonicalRepo: patchLabel.GetCanonicalPackage().GetCanonicalRepo().String(),
			Path:          patchLabel.GetRepoRelativePath(),
		})
		if !patchPropertiesValue.IsSet() {
			missingDependencies = true
			continue
		}
		if patchPropertiesValue.Message.Exists == nil {
			return PatchedRepoValue{}, fmt.Errorf("patch %#v does not exist", patchLabelStr)
		}

		patchContentsEntry, err := model_filesystem.NewFileContentsEntryFromProto(
			model_core.Nested(patchPropertiesValue, patchPropertiesValue.Message.Exists.Contents),
		)
		if err != nil {
			return PatchedRepoValue{}, fmt.Errorf("invalid file contents for patch %#v: %s", patchLabelStr, err)
		}

		patchesToApply = append(patchesToApply, patchToApply[TReference]{
			filename:          patchLabelStr,
			strip:             singleVersionOverridePatchStrip,
			fileContentsEntry: patchContentsEntry,
		})
	}

	if missingDependencies {
		return PatchedRepoValue{}, evaluation.ErrMissingDependency
	}
	if archiveContentsValue.Message.Exists == nil {
		return PatchedRepoValue{}, fmt.Errorf("file at URL %#v does not exist", sourceJSON.URL)
	}

	// TODO: Process singleVersionOverridePatchCommands!

	return c.applyPatches(
		ctx,
		e,
		model_core.Nested(archiveContentsValue, archiveContentsValue.Message.Exists.Contents),
		sourceJSON.StripPrefix,
		patchesToApply,
	)
}

type patchToApply[TReference any] struct {
	filename          string
	strip             int
	fileContentsEntry model_filesystem.FileContentsEntry[TReference]
}

type applyPatchesEnvironment[TReference object.BasicReference, TMetadata any] interface {
	model_core.ExistingObjectCapturer[TReference, TMetadata]

	GetDirectoryCreationParametersObjectValue(key *model_analysis_pb.DirectoryCreationParametersObject_Key) (*model_filesystem.DirectoryCreationParameters, bool)
	GetDirectoryReadersValue(key *model_analysis_pb.DirectoryReaders_Key) (*DirectoryReaders[TReference], bool)
	GetFileCreationParametersObjectValue(key *model_analysis_pb.FileCreationParametersObject_Key) (*model_filesystem.FileCreationParameters, bool)
	GetFileReaderValue(key *model_analysis_pb.FileReader_Key) (*model_filesystem.FileReader[TReference], bool)
}

func (c *baseComputer[TReference, TMetadata]) applyPatches(
	ctx context.Context,
	e applyPatchesEnvironment[TReference, TMetadata],
	rootRef model_core.Message[*model_filesystem_pb.DirectoryReference, TReference],
	stripPrefix string,
	patches []patchToApply[TReference],
) (PatchedRepoValue, error) {
	fileReader, gotFileReader := e.GetFileReaderValue(&model_analysis_pb.FileReader_Key{})
	directoryCreationParameters, gotDirectoryCreationParameters := e.GetDirectoryCreationParametersObjectValue(&model_analysis_pb.DirectoryCreationParametersObject_Key{})
	directoryReaders, gotDirectoryReaders := e.GetDirectoryReadersValue(&model_analysis_pb.DirectoryReaders_Key{})
	fileCreationParameters, gotFileCreationParameters := e.GetFileCreationParametersObjectValue(&model_analysis_pb.FileCreationParametersObject_Key{})
	if !gotFileReader || !gotDirectoryCreationParameters || !gotDirectoryReaders || !gotFileCreationParameters {
		return PatchedRepoValue{}, evaluation.ErrMissingDependency
	}

	rootDirectory := &changeTrackingDirectory[TReference]{
		currentReference: rootRef,
	}

	// Strip the provided directory prefix.
	loadOptions := &changeTrackingDirectoryLoadOptions[TReference]{
		context:         ctx,
		directoryReader: directoryReaders.Directory,
		leavesReader:    directoryReaders.Leaves,
	}
	rootDirectoryResolver := changeTrackingDirectoryResolver[TReference]{
		loadOptions:      loadOptions,
		currentDirectory: rootDirectory,
	}
	if err := path.Resolve(
		path.UNIXFormat.NewParser(stripPrefix),
		path.NewRelativeScopeWalker(&rootDirectoryResolver),
	); err != nil {
		return PatchedRepoValue{}, fmt.Errorf("failed to strip prefix %#v from contents: %w", stripPrefix, err)
	}
	rootDirectory = rootDirectoryResolver.currentDirectory

	patchedFiles, err := c.filePool.NewFile()
	if err != nil {
		return PatchedRepoValue{}, err
	}
	defer patchedFiles.Close()
	patchedFilesWriter := model_filesystem.NewSectionWriter(patchedFiles)

	for _, patch := range patches {
		err = c.applyPatch(
			ctx,
			rootDirectory,
			loadOptions,
			patch.strip,
			fileReader,
			func() (io.Reader, error) {
				return fileReader.FileOpenRead(ctx, patch.fileContentsEntry, 0), nil
			},
			patchedFiles,
			patchedFilesWriter,
		)
		if err != nil {
			return PatchedRepoValue{}, fmt.Errorf("patch %q: %w", patch.filename, err)
		}
	}

	return c.returnRepoMerkleTree(
		ctx,
		e,
		rootDirectory,
		directoryCreationParameters,
		directoryReaders,
		fileCreationParameters,
		patchedFiles,
	)
}

func (c *baseComputer[TReference, TMetadata]) applyPatch(
	ctx context.Context,
	rootDirectory *changeTrackingDirectory[TReference],
	loadOptions *changeTrackingDirectoryLoadOptions[TReference],
	patchStrip int,
	fileReader *model_filesystem.FileReader[TReference],
	openFile func() (io.Reader, error),
	patchedFiles filesystem.FileReader,
	patchedFilesWriter *model_filesystem.SectionWriter,
) error {
	patchedFile, err := openFile()
	if err != nil {
		return fmt.Errorf("open patched file contents: %s", err)
	}

	files, _, err := gitdiff.Parse(patchedFile)
	if err != nil {
		return fmt.Errorf("invalid patch: %w", err)
	}

	for _, file := range files {
		var fileContents changeTrackingFileContents[TReference]
		isExecutable := false
		if !file.IsNew {
			r := &changeTrackingDirectoryExistingFileResolver[TReference]{
				loadOptions: loadOptions,
				stack:       util.NewNonEmptyStack(rootDirectory),
			}
			if err := path.Resolve(
				path.UNIXFormat.NewParser(file.OldName),
				path.NewRelativeScopeWalker(
					newStrippingComponentWalker(r, patchStrip),
				),
			); err != nil {
				return fmt.Errorf("cannot resolve path %#v: %w", file.OldName, err)
			}
			f, err := r.getFile()
			if err != nil {
				return fmt.Errorf("cannot get file at path %#v: %w", file.OldName, err)
			}
			fileContents = f.contents
			isExecutable = f.isExecutable
		}

		// Compute the offsets at which changes need to
		// be made to the file.
		var srcScan io.Reader
		if fileContents == nil {
			srcScan = bytes.NewBuffer(nil)
		} else {
			srcScan, err = fileContents.openRead(ctx, fileReader, patchedFiles)
			if err != nil {
				return fmt.Errorf("failed to open file %#v: %s", file.OldName, err)
			}
		}
		fragmentsOffsetsBytes, err := diff.FindTextFragmentOffsetsBytes(file.TextFragments, bufio.NewReader(srcScan))
		if err != nil {
			return fmt.Errorf("failed to apply to file %#v: %w", file.OldName, err)
		}

		var srcReplace io.Reader
		if fileContents == nil {
			srcReplace = bytes.NewBuffer(nil)
		} else {
			srcReplace, err = fileContents.openRead(ctx, fileReader, patchedFiles)
			if err != nil {
				return fmt.Errorf("failed to open file %#v: %w", file.OldName, err)
			}
		}

		patchedFileOffsetBytes := patchedFilesWriter.GetOffsetBytes()
		if err := diff.ReplaceTextFragments(patchedFilesWriter, srcReplace, file.TextFragments, fragmentsOffsetsBytes); err != nil {
			return fmt.Errorf("failed to replace text fragments to %#v: %w", file.OldName, err)
		}

		r := &changeTrackingDirectoryNewFileResolver[TReference]{
			loadOptions: loadOptions,
			stack:       util.NewNonEmptyStack(rootDirectory),
		}
		if err := path.Resolve(
			path.UNIXFormat.NewParser(file.NewName),
			path.NewRelativeScopeWalker(
				newStrippingComponentWalker(r, patchStrip),
			),
		); err != nil {
			return fmt.Errorf("cannot resolve path %#v: %w", file.NewName, err)
		}
		if r.TerminalName == nil {
			return fmt.Errorf("path %#v does not resolve to a file", file.NewName)
		}

		if file.NewMode != 0 {
			isExecutable = file.NewMode&0o111 != 0
		}
		if err := r.stack.Peek().setFile(
			loadOptions,
			*r.TerminalName,
			&changeTrackingFile[TReference]{
				isExecutable: isExecutable,
				contents: patchedFileContents[TReference]{
					offsetBytes: patchedFileOffsetBytes,
					sizeBytes:   patchedFilesWriter.GetOffsetBytes() - patchedFileOffsetBytes,
				},
			},
		); err != nil {
			return err
		}
	}
	return nil
}

// newRepositoryOS creates a repository_os object that can be embedded
// into module_ctx and repository_ctx objects.
func newRepositoryOS[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata](thread *starlark.Thread, repoPlatform *model_analysis_pb.RegisteredRepoPlatform_Value) starlark.Value {
	environ := starlark.NewDict(len(repoPlatform.RepositoryOsEnviron))
	for _, entry := range repoPlatform.RepositoryOsEnviron {
		environ.SetKey(thread, starlark.String(entry.Name), starlark.String(entry.Value))
	}
	s := model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
		"arch":    starlark.String(repoPlatform.RepositoryOsArch),
		"environ": environ,
		"name":    starlark.String(repoPlatform.RepositoryOsName),
	})
	s.Freeze()
	return s
}

type moduleOrRepositoryContextEnvironment[TReference object.BasicReference, TMetadata any] interface {
	model_core.ExistingObjectCapturer[TReference, TMetadata]

	GetActionResultValue(model_core.PatchedMessage[*model_analysis_pb.ActionResult_Key, dag.ObjectContentsWalker]) model_core.Message[*model_analysis_pb.ActionResult_Value, TReference]
	GetCommandEncoderObjectValue(*model_analysis_pb.CommandEncoderObject_Key) (model_encoding.BinaryEncoder, bool)
	GetDirectoryCreationParametersObjectValue(*model_analysis_pb.DirectoryCreationParametersObject_Key) (*model_filesystem.DirectoryCreationParameters, bool)
	GetDirectoryCreationParametersValue(*model_analysis_pb.DirectoryCreationParameters_Key) model_core.Message[*model_analysis_pb.DirectoryCreationParameters_Value, TReference]
	GetDirectoryReadersValue(key *model_analysis_pb.DirectoryReaders_Key) (*DirectoryReaders[TReference], bool)
	GetFileCreationParametersObjectValue(*model_analysis_pb.FileCreationParametersObject_Key) (*model_filesystem.FileCreationParameters, bool)
	GetFileCreationParametersValue(*model_analysis_pb.FileCreationParameters_Key) model_core.Message[*model_analysis_pb.FileCreationParameters_Value, TReference]
	GetFileReaderValue(*model_analysis_pb.FileReader_Key) (*model_filesystem.FileReader[TReference], bool)
	GetHttpArchiveContentsValue(*model_analysis_pb.HttpArchiveContents_Key) model_core.Message[*model_analysis_pb.HttpArchiveContents_Value, TReference]
	GetHttpFileContentsValue(*model_analysis_pb.HttpFileContents_Key) model_core.Message[*model_analysis_pb.HttpFileContents_Value, TReference]
	GetRegisteredRepoPlatformValue(*model_analysis_pb.RegisteredRepoPlatform_Key) model_core.Message[*model_analysis_pb.RegisteredRepoPlatform_Value, TReference]
	GetRepoValue(*model_analysis_pb.Repo_Key) model_core.Message[*model_analysis_pb.Repo_Value, TReference]
	GetRootModuleValue(*model_analysis_pb.RootModule_Key) model_core.Message[*model_analysis_pb.RootModule_Value, TReference]
	GetStableInputRootPathObjectValue(*model_analysis_pb.StableInputRootPathObject_Key) (*model_starlark.BarePath, bool)
}

type moduleOrRepositoryContext[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	computer               *baseComputer[TReference, TMetadata]
	context                context.Context
	environment            moduleOrRepositoryContextEnvironment[TReference, TMetadata]
	subdirectoryComponents []path.Component

	commandEncoder                     model_encoding.BinaryEncoder
	defaultWorkingDirectoryPath        *model_starlark.BarePath
	directoryCreationParameters        *model_filesystem.DirectoryCreationParameters
	directoryCreationParametersMessage *model_filesystem_pb.DirectoryCreationParameters
	directoryReaders                   *DirectoryReaders[TReference]
	directoryLoadOptions               *changeTrackingDirectoryLoadOptions[TReference]
	fileCreationParameters             *model_filesystem.FileCreationParameters
	fileCreationParametersMessage      *model_filesystem_pb.FileCreationParameters
	fileReader                         *model_filesystem.FileReader[TReference]
	pathUnpackerInto                   unpack.UnpackerInto[*model_starlark.BarePath]
	repoPlatform                       model_core.Message[*model_analysis_pb.RegisteredRepoPlatform_Value, TReference]
	virtualRootScopeWalkerFactory      *path.VirtualRootScopeWalkerFactory

	inputRootDirectory *changeTrackingDirectory[TReference]
	patchedFiles       filesystem.FileReader
	patchedFilesWriter *model_filesystem.SectionWriter
}

func (c *baseComputer[TReference, TMetadata]) newModuleOrRepositoryContext(ctx context.Context, e moduleOrRepositoryContextEnvironment[TReference, TMetadata], subdirectoryComponents []path.Component) (*moduleOrRepositoryContext[TReference, TMetadata], error) {
	return &moduleOrRepositoryContext[TReference, TMetadata]{
		computer:               c,
		context:                ctx,
		environment:            e,
		subdirectoryComponents: subdirectoryComponents,

		inputRootDirectory: &changeTrackingDirectory[TReference]{},
	}, nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) release() {
	if mrc.patchedFiles != nil {
		mrc.patchedFiles.Close()
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) resolveRepoDirectory() (*changeTrackingDirectory[TReference], error) {
	repoDirectory := mrc.inputRootDirectory
	for _, component := range mrc.subdirectoryComponents {
		childDirectory, ok := repoDirectory.directories[component]
		if !ok {
			return nil, errors.New("repository rule removed its own repository directory")
		}
		repoDirectory = childDirectory
	}
	return repoDirectory, nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetCommandEncoder() {
	if mrc.commandEncoder == nil {
		if v, ok := mrc.environment.GetCommandEncoderObjectValue(&model_analysis_pb.CommandEncoderObject_Key{}); ok {
			mrc.commandEncoder = v
		}
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetDirectoryCreationParameters() {
	mrc.maybeGetDirectoryReaders()
	if mrc.directoryCreationParameters == nil {
		if v, ok := mrc.environment.GetDirectoryCreationParametersObjectValue(&model_analysis_pb.DirectoryCreationParametersObject_Key{}); ok {
			mrc.directoryCreationParameters = v
		}
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetDirectoryCreationParametersMessage() {
	if mrc.directoryCreationParametersMessage == nil {
		if v := mrc.environment.GetDirectoryCreationParametersValue(&model_analysis_pb.DirectoryCreationParameters_Key{}); v.IsSet() {
			mrc.directoryCreationParametersMessage = v.Message.DirectoryCreationParameters
		}
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetDirectoryReaders() {
	if mrc.directoryReaders == nil {
		if v, ok := mrc.environment.GetDirectoryReadersValue(&model_analysis_pb.DirectoryReaders_Key{}); ok {
			mrc.directoryReaders = v
			mrc.directoryLoadOptions = &changeTrackingDirectoryLoadOptions[TReference]{
				context:         mrc.context,
				directoryReader: v.Directory,
				leavesReader:    v.Leaves,
			}
		}
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetFileCreationParameters() {
	if mrc.fileCreationParameters == nil {
		if v, ok := mrc.environment.GetFileCreationParametersObjectValue(&model_analysis_pb.FileCreationParametersObject_Key{}); ok {
			mrc.fileCreationParameters = v
		}
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetFileCreationParametersMessage() {
	if mrc.fileCreationParametersMessage == nil {
		if v := mrc.environment.GetFileCreationParametersValue(&model_analysis_pb.FileCreationParameters_Key{}); v.IsSet() {
			mrc.fileCreationParametersMessage = v.Message.FileCreationParameters
		}
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetFileReader() {
	if mrc.fileReader == nil {
		if v, ok := mrc.environment.GetFileReaderValue(&model_analysis_pb.FileReader_Key{}); ok {
			mrc.fileReader = v
		}
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeInitializePatchedFiles() error {
	if mrc.patchedFiles == nil {
		patchedFiles, err := mrc.computer.filePool.NewFile()
		if err != nil {
			return err
		}
		mrc.patchedFiles = patchedFiles
		mrc.patchedFilesWriter = model_filesystem.NewSectionWriter(patchedFiles)
	}
	return nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetRepoPlatform() {
	if !mrc.repoPlatform.IsSet() {
		mrc.repoPlatform = mrc.environment.GetRegisteredRepoPlatformValue(&model_analysis_pb.RegisteredRepoPlatform_Key{})
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) maybeGetStableInputRootPath() error {
	if mrc.virtualRootScopeWalkerFactory == nil {
		stableInputRootPath, ok := mrc.environment.GetStableInputRootPathObjectValue(&model_analysis_pb.StableInputRootPathObject_Key{})
		if !ok {
			return evaluation.ErrMissingDependency
		}

		defaultWorkingDirectoryPath := stableInputRootPath
		for _, component := range mrc.subdirectoryComponents {
			defaultWorkingDirectoryPath = defaultWorkingDirectoryPath.Append(component)
		}

		virtualRootScopeWalkerFactory, err := path.NewVirtualRootScopeWalkerFactory(stableInputRootPath, nil)
		if err != nil {
			return err
		}

		mrc.defaultWorkingDirectoryPath = defaultWorkingDirectoryPath
		externalPath := stableInputRootPath.Append(model_starlark.ComponentExternal)
		mrc.pathUnpackerInto = &externalRepoAddingPathUnpackerInto[TReference, TMetadata]{
			context: mrc,
			base: model_starlark.NewPathOrLabelOrStringUnpackerInto[TReference, TMetadata](
				func(canonicalRepo label.CanonicalRepo) (*model_starlark.BarePath, error) {
					// Map labels to paths under external/${repo}.
					return externalPath.Append(path.MustNewComponent(canonicalRepo.String())), nil
				},
				defaultWorkingDirectoryPath,
			),
			externalPath: externalPath,
		}
		mrc.virtualRootScopeWalkerFactory = virtualRootScopeWalkerFactory
	}
	return nil
}

func createDownloadSuccessResult[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata](integrity *model_analysis_pb.SubresourceIntegrity, sha256 []byte) starlark.Value {
	fields := map[string]any{
		"success": starlark.Bool(true),
	}

	if integrity == nil {
		integrity = &model_analysis_pb.SubresourceIntegrity{
			HashAlgorithm: model_analysis_pb.SubresourceIntegrity_SHA256,
			Hash:          sha256,
		}
	}
	fields["integrity"] = starlark.String(strings.ToLower(model_analysis_pb.SubresourceIntegrity_HashAlgorithm_name[int32(integrity.HashAlgorithm)]) + "-" + base64.StdEncoding.EncodeToString(integrity.Hash))
	if integrity.HashAlgorithm == model_analysis_pb.SubresourceIntegrity_SHA256 {
		fields["sha256"] = starlark.String(hex.EncodeToString(integrity.Hash))
	}

	return model_starlark.NewStructFromDict[TReference, TMetadata](nil, fields)
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doDownload(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	mrc.maybeGetDirectoryReaders()
	if err := mrc.maybeGetStableInputRootPath(); err != nil {
		return nil, err
	}
	if mrc.directoryLoadOptions == nil {
		return nil, evaluation.ErrMissingDependency
	}

	if len(args) > 8 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want at most 8", b.Name(), len(args))
	}
	var urls []string
	output := mrc.defaultWorkingDirectoryPath
	sha256 := ""
	executable := false
	allowFail := false
	canonicalID := ""
	var auth map[string]map[string]string
	var headers map[string]string
	integrity := ""
	block := true
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"url", unpack.Bind(thread, &urls, unpack.Or([]unpack.UnpackerInto[[]string]{
			unpack.List(unpack.Stringer(unpack.URL)),
			unpack.Singleton(unpack.Stringer(unpack.URL)),
		})),
		"output?", unpack.Bind(thread, &output, mrc.pathUnpackerInto),
		"sha256?", unpack.Bind(thread, &sha256, unpack.String),
		"executable?", unpack.Bind(thread, &executable, unpack.Bool),
		"allow_fail?", unpack.Bind(thread, &allowFail, unpack.Bool),
		"canonical_id?", unpack.Bind(thread, &canonicalID, unpack.String),
		"auth?", unpack.Bind(thread, &auth, unpack.Dict(unpack.Stringer(unpack.URL), unpack.Dict(unpack.String, unpack.String))),
		"headers?", unpack.Bind(thread, &headers, unpack.Dict(unpack.String, unpack.String)),
		"integrity?", unpack.Bind(thread, &integrity, unpack.String),
		"block?", unpack.Bind(thread, &block, unpack.Bool),
	); err != nil {
		return nil, err
	}

	integrityMessage, err := parseSubresourceIntegrityOrSHA256(integrity, sha256)
	if err != nil {
		return nil, err
	}

	headersEntries := make([]*model_analysis_pb.HttpFetchOptions_Header, 0, len(headers))
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		headersEntries = append(headersEntries, &model_analysis_pb.HttpFetchOptions_Header{
			Name:  name,
			Value: headers[name],
		})
	}

	fileContentsValue := mrc.environment.GetHttpFileContentsValue(&model_analysis_pb.HttpFileContents_Key{
		FetchOptions: &model_analysis_pb.HttpFetchOptions{
			Urls:      urls,
			Integrity: integrityMessage,
			AllowFail: allowFail,
			Headers:   headersEntries,
			// TODO: Set auth!
		},
	})

	// Depending on whether "block" is set to true or not,
	// immediately attempt to insert the downloaded file into the
	// file system or delay it until result.wait() is called.
	if block {
		return mrc.completeDownload(output, executable, integrityMessage, fileContentsValue)
	}
	return model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
		"wait": starlark.NewBuiltin("repository_ctx.download.wait", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			return mrc.completeDownload(output, executable, integrityMessage, fileContentsValue)
		}),
	}), nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) completeDownload(output *model_starlark.BarePath, executable bool, integrity *model_analysis_pb.SubresourceIntegrity, fileContentsValue model_core.Message[*model_analysis_pb.HttpFileContents_Value, TReference]) (starlark.Value, error) {
	if !fileContentsValue.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	exists := fileContentsValue.Message.Exists
	if exists == nil {
		// File does not exist, or allow_fail was set and an
		// error occurred.
		return model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
			"success": starlark.Bool(false),
		}), nil
	}

	// Insert the downloaded file into the file system.
	r := &changeTrackingDirectoryNewFileResolver[TReference]{
		loadOptions: mrc.directoryLoadOptions,
		stack:       util.NewNonEmptyStack(mrc.inputRootDirectory),
	}
	if err := path.Resolve(output, mrc.virtualRootScopeWalkerFactory.New(r)); err != nil {
		return nil, fmt.Errorf("cannot resolve %#v: %w", output.GetUNIXString(), err)
	}
	if r.TerminalName == nil {
		return nil, fmt.Errorf("%#v does not resolve to a file", output.GetUNIXString())
	}

	if err := r.stack.Peek().setFile(
		mrc.directoryLoadOptions,
		*r.TerminalName,
		&changeTrackingFile[TReference]{
			isExecutable: executable,
			contents: unmodifiedFileContents[TReference]{
				contents: model_core.Nested(fileContentsValue, exists.Contents),
			},
		},
	); err != nil {
		return nil, err
	}

	return createDownloadSuccessResult[TReference, TMetadata](integrity, exists.Sha256), nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doDownloadAndExtract(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	mrc.maybeGetDirectoryReaders()
	if err := mrc.maybeGetStableInputRootPath(); err != nil {
		return nil, err
	}
	if mrc.directoryLoadOptions == nil {
		return nil, evaluation.ErrMissingDependency
	}

	if len(args) > 9 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want at most 9", b.Name(), len(args))
	}
	var urls []string
	output := mrc.defaultWorkingDirectoryPath
	sha256 := ""
	typeStr := ""
	var stripPrefix path.Parser = &path.EmptyBuilder
	allowFail := false
	canonicalID := ""
	var auth map[string]map[string]string
	var headers map[string]string
	integrity := ""
	var renameFiles map[string]string
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"url", unpack.Bind(thread, &urls, unpack.Or([]unpack.UnpackerInto[[]string]{
			unpack.List(unpack.Stringer(unpack.URL)),
			unpack.Singleton(unpack.Stringer(unpack.URL)),
		})),
		"output?", unpack.Bind(thread, &output, mrc.pathUnpackerInto),
		"sha256?", unpack.Bind(thread, &sha256, unpack.String),
		"type?", unpack.Bind(thread, &typeStr, unpack.String),
		"strip_prefix?", unpack.Bind(thread, &stripPrefix, unpack.PathParser(path.UNIXFormat)),
		"allow_fail?", unpack.Bind(thread, &allowFail, unpack.Bool),
		"canonical_id?", unpack.Bind(thread, &canonicalID, unpack.String),
		"auth?", unpack.Bind(thread, &auth, unpack.Dict(unpack.Stringer(unpack.URL), unpack.Dict(unpack.String, unpack.String))),
		"headers?", unpack.Bind(thread, &headers, unpack.Dict(unpack.String, unpack.String)),
		"integrity?", unpack.Bind(thread, &integrity, unpack.String),
		"rename_files?", unpack.Bind(thread, &renameFiles, unpack.Dict(unpack.String, unpack.String)),
		// For compatibility with Bazel < 8.
		"stripPrefix?", unpack.Bind(thread, &stripPrefix, unpack.PathParser(path.UNIXFormat)),
	); err != nil {
		return nil, err
	}

	integrityMessage, err := parseSubresourceIntegrityOrSHA256(integrity, sha256)
	if err != nil {
		return nil, err
	}

	var typeToMatch string
	if typeStr != "" {
		typeToMatch = "." + typeStr
	} else if len(urls) > 0 {
		typeToMatch = urls[0]
	} else {
		return nil, errors.New("no URLs provided")
	}
	archiveFormat, ok := inferArchiveFormatFromURL(typeToMatch)
	if !ok {
		return nil, fmt.Errorf("cannot derive archive format from file extension of %#v", typeToMatch)
	}

	archiveContentsValue := mrc.environment.GetHttpArchiveContentsValue(&model_analysis_pb.HttpArchiveContents_Key{
		FetchOptions: &model_analysis_pb.HttpFetchOptions{
			AllowFail: allowFail,
			Integrity: integrityMessage,
			Urls:      urls,
		},
		Format: archiveFormat,
	})
	if !archiveContentsValue.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	if archiveContentsValue.Message.Exists == nil {
		// File does not exist, or allow_fail was set and an
		// error occurred.
		return model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
			"success": starlark.Bool(false),
		}), nil
	}

	// Determine which directory to place inside the file system.
	archiveRootDirectory := changeTrackingDirectory[TReference]{
		currentReference: model_core.Nested(archiveContentsValue, archiveContentsValue.Message.Exists.Contents),
	}
	rootDirectoryResolver := changeTrackingDirectoryResolver[TReference]{
		loadOptions:      mrc.directoryLoadOptions,
		currentDirectory: &archiveRootDirectory,
	}
	if err := path.Resolve(stripPrefix, path.NewRelativeScopeWalker(&rootDirectoryResolver)); err != nil {
		return nil, errors.New("failed to strip prefix from contents")
	}

	// Insert the directory into the file system.
	r := &changeTrackingDirectoryNewDirectoryResolver[TReference]{
		loadOptions: mrc.directoryLoadOptions,
		stack:       util.NewNonEmptyStack(mrc.inputRootDirectory),
	}
	if err := path.Resolve(output, mrc.virtualRootScopeWalkerFactory.New(r)); err != nil {
		return nil, fmt.Errorf("cannot resolve %#v: %w", output.GetUNIXString(), err)
	}

	*r.stack.Peek() = *rootDirectoryResolver.currentDirectory

	return createDownloadSuccessResult[TReference, TMetadata](integrityMessage, archiveContentsValue.Message.Exists.Sha256), nil
}

// bytesToValidString converts a byte slice containing UTF-8 encoded
// characters to a string. If invalid UTF-8 sequences are encountered,
// U+FFFD is emitted.
func bytesToValidString(p []byte) string {
	if utf8.Valid(p) {
		// Fast path: byte slice is already valid UTF-8.
		return string(p)
	}

	// Slow path: byte slice contains one or more invalid sequences.
	var sb strings.Builder
	for {
		r, size := utf8.DecodeRune(p)
		if size == 0 {
			return sb.String()
		}
		sb.WriteRune(r)
		p = p[size:]
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doExecute(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	mrc.maybeGetCommandEncoder()
	mrc.maybeGetDirectoryCreationParameters()
	mrc.maybeGetDirectoryCreationParametersMessage()
	mrc.maybeGetDirectoryReaders()
	mrc.maybeGetFileCreationParameters()
	mrc.maybeGetFileCreationParametersMessage()
	mrc.maybeGetFileReader()
	mrc.maybeGetRepoPlatform()
	stableInputRootPathError := mrc.maybeGetStableInputRootPath()
	if mrc.commandEncoder == nil ||
		mrc.directoryCreationParameters == nil ||
		mrc.directoryCreationParametersMessage == nil ||
		mrc.directoryReaders == nil ||
		mrc.fileCreationParameters == nil ||
		mrc.fileCreationParametersMessage == nil ||
		mrc.fileReader == nil ||
		!mrc.repoPlatform.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	if stableInputRootPathError != nil {
		return nil, stableInputRootPathError
	}

	var arguments []any
	timeout := int64(600)
	environment := map[string]string{}
	quiet := true
	workingDirectory := mrc.defaultWorkingDirectoryPath
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"arguments", unpack.Bind(thread, &arguments, unpack.List(
			unpack.Or([]unpack.UnpackerInto[any]{
				unpack.Decay(unpack.String),
				unpack.Decay(mrc.pathUnpackerInto),
			}),
		)),
		"timeout?", unpack.Bind(thread, &timeout, unpack.Int[int64]()),
		"environment?", unpack.Bind(thread, &environment, unpack.Dict(unpack.String, unpack.String)),
		"quiet?", unpack.Bind(thread, &quiet, unpack.Bool),
		"working_directory?", unpack.Bind(thread, &workingDirectory, mrc.pathUnpackerInto),
	); err != nil {
		return nil, err
	}

	// Inherit environment variables from
	// the repo platform.
	for _, environmentVariable := range mrc.repoPlatform.Message.RepositoryOsEnviron {
		if _, ok := environment[environmentVariable.Name]; !ok {
			environment[environmentVariable.Name] = environmentVariable.Value
		}
	}

	// Convert arguments and environment
	// variables to B-trees, so that they can
	// be attached to the Command message.
	referenceFormat := mrc.computer.getReferenceFormat()
	argumentsBuilder := btree.NewSplitProllyBuilder(
		1<<16,
		1<<18,
		btree.NewObjectCreatingNodeMerger(
			mrc.commandEncoder,
			referenceFormat,
			/* parentNodeComputer = */ func(createdObject model_core.CreatedObject[dag.ObjectContentsWalker], childNodes []*model_command_pb.ArgumentList_Element) (model_core.PatchedMessage[*model_command_pb.ArgumentList_Element, dag.ObjectContentsWalker], error) {
				patcher := model_core.NewReferenceMessagePatcher[dag.ObjectContentsWalker]()
				return model_core.NewPatchedMessage(
					&model_command_pb.ArgumentList_Element{
						Level: &model_command_pb.ArgumentList_Element_Parent{
							Parent: patcher.AddReference(
								createdObject.Contents.GetReference(),
								dag.NewSimpleObjectContentsWalker(createdObject.Contents, createdObject.Metadata),
							),
						},
					},
					patcher,
				), nil
			},
		),
	)
	for _, argument := range arguments {
		var argumentStr string
		switch typedArgument := argument.(type) {
		case string:
			argumentStr = typedArgument
		case *model_starlark.BarePath:
			argumentStr = typedArgument.GetUNIXString()
		default:
			panic("unexpected argument type")
		}
		if err := argumentsBuilder.PushChild(
			model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_command_pb.ArgumentList_Element{
				Level: &model_command_pb.ArgumentList_Element_Leaf{
					Leaf: argumentStr,
				},
			}),
		); err != nil {
			return nil, err
		}
	}
	argumentList, err := argumentsBuilder.FinalizeList()
	if err != nil {
		return nil, err
	}

	environmentVariableList, err := mrc.computer.convertDictToEnvironmentVariableList(environment, mrc.commandEncoder)
	if err != nil {
		return nil, err
	}

	// The working directory should be implicitly created.
	if err := path.Resolve(
		workingDirectory,
		mrc.virtualRootScopeWalkerFactory.New(
			&changeTrackingDirectoryNewDirectoryResolver[TReference]{
				loadOptions: mrc.directoryLoadOptions,
				stack:       util.NewNonEmptyStack(mrc.inputRootDirectory),
			},
		),
	); err != nil {
		return nil, fmt.Errorf("failed to create working directory: %w", err)
	}

	// The command to execute is permitted to make changes to the
	// contents of the repo. These changes should be carried over to
	// any subsequent command and also become part of the repo's
	// final contents. Construct a pattern for capturing the
	// directory in the input root belonging to the repo.
	//
	// TODO: Should this used inlinedtree? Likely doesn't make
	// sense, considering that the pattern is so simple.
	outputPathPattern := &model_command_pb.PathPattern{}
	for i := len(mrc.subdirectoryComponents); i > 0; i-- {
		outputPathPattern = &model_command_pb.PathPattern{
			Children: &model_command_pb.PathPattern_ChildrenInline{
				ChildrenInline: &model_command_pb.PathPattern_Children{
					Children: []*model_command_pb.PathPattern_Child{{
						Name:    mrc.subdirectoryComponents[i-1].String(),
						Pattern: outputPathPattern,
					}},
				},
			},
		}
	}

	commandPatcher := argumentList.Patcher
	commandPatcher.Merge(environmentVariableList.Patcher)
	createdCommand, err := model_core.MarshalAndEncodePatchedMessage(
		model_core.NewPatchedMessage(
			&model_command_pb.Command{
				Arguments:                   argumentList.Message,
				EnvironmentVariables:        environmentVariableList.Message,
				DirectoryCreationParameters: mrc.directoryCreationParametersMessage,
				FileCreationParameters:      mrc.fileCreationParametersMessage,
				OutputPathPattern:           outputPathPattern,
				WorkingDirectory:            workingDirectory.GetUNIXString(),
				NeedsStableInputRootPath:    true,
			},
			commandPatcher,
		),
		referenceFormat,
		mrc.commandEncoder,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create command: %w", err)
	}

	inputRootReference, err := mrc.computer.createMerkleTreeFromChangeTrackingDirectory(mrc.context, mrc.environment, mrc.inputRootDirectory, mrc.directoryCreationParameters, mrc.directoryReaders, mrc.fileCreationParameters, mrc.patchedFiles)
	if err != nil {
		return nil, fmt.Errorf("failed to create Merkle tree of root directory: %w", err)
	}

	// Execute the command.
	keyPatcher := inputRootReference.Patcher
	actionResult := mrc.environment.GetActionResultValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ActionResult_Key{
				PlatformPkixPublicKey: mrc.repoPlatform.Message.ExecPkixPublicKey,
				CommandReference: keyPatcher.AddReference(
					createdCommand.Contents.GetReference(),
					dag.NewSimpleObjectContentsWalker(createdCommand.Contents, createdCommand.Metadata),
				),
				InputRootReference: inputRootReference.Message.Reference,
				ExecutionTimeout:   &durationpb.Duration{Seconds: timeout},
			},
			keyPatcher,
		),
	)
	if !actionResult.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}

	// Extract standard output and standard error from the results.
	outputs, err := model_parser.MaybeDereference(mrc.context, mrc.directoryReaders.CommandOutputs, model_core.Nested(actionResult, actionResult.Message.OutputsReference))
	if err != nil {
		return nil, fmt.Errorf("failed to obtain outputs from action result: %w", err)
	}

	stdoutEntry, err := model_filesystem.NewFileContentsEntryFromProto(
		model_core.Nested(outputs, outputs.Message.GetStdout()),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid standard output entry: %w", err)
	}
	stdout, err := mrc.fileReader.FileReadAll(mrc.context, stdoutEntry, 1<<22)
	if err != nil {
		return nil, fmt.Errorf("failed to read standard output: %w", err)
	}

	stderrEntry, err := model_filesystem.NewFileContentsEntryFromProto(
		model_core.Nested(outputs, outputs.Message.GetStderr()),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid standard error entry: %w", err)
	}
	stderr, err := mrc.fileReader.FileReadAll(mrc.context, stderrEntry, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("failed to read standard error: %w", err)
	}

	// The command may have mutated the repo's contents. Extract the
	// repo directory contents from the results and copy it into the
	// input root.
	var outputRootDirectory changeTrackingDirectory[TReference]
	if err := outputRootDirectory.setContents(
		model_core.Nested(outputs, outputs.Message.GetOutputRoot()),
		mrc.directoryLoadOptions,
	); err != nil {
		return nil, fmt.Errorf("failed load output root: %w", err)
	}
	inputRepoDirectory := mrc.inputRootDirectory
	outputRepoDirectory := &outputRootDirectory
	for _, component := range mrc.subdirectoryComponents {
		if err := inputRepoDirectory.maybeLoadContents(mrc.directoryLoadOptions); err != nil {
			return nil, err
		}
		if err := outputRepoDirectory.maybeLoadContents(mrc.directoryLoadOptions); err != nil {
			return nil, err
		}
		var ok bool
		inputRepoDirectory, ok = inputRepoDirectory.directories[component]
		if !ok {
			return nil, fmt.Errorf("repo directory no longer exists")
		}
		outputRepoDirectory, ok = outputRepoDirectory.directories[component]
		if !ok {
			return nil, fmt.Errorf("repo directory no longer exists")
		}
	}
	*inputRepoDirectory = *outputRepoDirectory

	return model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
		"return_code": starlark.MakeInt64(actionResult.Message.ExitCode),
		"stderr":      starlark.String(bytesToValidString(stderr)),
		"stdout":      starlark.String(bytesToValidString(stdout)),
	}), nil
}

func (moduleOrRepositoryContext[TReference, TMetadata]) doExtract(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return nil, errors.New("TODO: Implement!")
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doFile(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	mrc.maybeGetDirectoryReaders()
	stableInputRootPathError := mrc.maybeGetStableInputRootPath()
	if mrc.directoryLoadOptions == nil {
		return nil, evaluation.ErrMissingDependency
	}
	if stableInputRootPathError != nil {
		return nil, stableInputRootPathError
	}

	var filePath *model_starlark.BarePath
	content := ""
	executable := true
	legacyUTF8 := true
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"path", unpack.Bind(thread, &filePath, mrc.pathUnpackerInto),
		"content?", unpack.Bind(thread, &content, unpack.String),
		"executable?", unpack.Bind(thread, &executable, unpack.Bool),
		"legacy_utf8?", unpack.Bind(thread, &legacyUTF8, unpack.Bool),
	); err != nil {
		return nil, err
	}

	r := &changeTrackingDirectoryNewFileResolver[TReference]{
		loadOptions: mrc.directoryLoadOptions,
		stack:       util.NewNonEmptyStack(mrc.inputRootDirectory),
	}
	if err := path.Resolve(filePath, mrc.virtualRootScopeWalkerFactory.New(r)); err != nil {
		return nil, fmt.Errorf("cannot resolve %#v: %w", filePath.GetUNIXString(), err)
	}
	if r.TerminalName == nil {
		return nil, fmt.Errorf("%#v does not resolve to a file", filePath.GetUNIXString())
	}

	if err := mrc.maybeInitializePatchedFiles(); err != nil {
		return nil, err
	}

	// TODO: Do UTF-8 -> ISO-8859-1
	// conversion if legacy_utf8=False.
	patchedFileOffsetBytes := mrc.patchedFilesWriter.GetOffsetBytes()
	if _, err := mrc.patchedFilesWriter.WriteString(content); err != nil {
		return nil, fmt.Errorf("failed to write to file at %#v: %w", filePath.GetUNIXString(), err)
	}

	if err := r.stack.Peek().setFile(
		mrc.directoryLoadOptions,
		*r.TerminalName,
		&changeTrackingFile[TReference]{
			isExecutable: executable,
			contents: patchedFileContents[TReference]{
				offsetBytes: patchedFileOffsetBytes,
				sizeBytes:   mrc.patchedFilesWriter.GetOffsetBytes() - patchedFileOffsetBytes,
			},
		},
	); err != nil {
		return nil, err
	}

	return starlark.None, nil
}

func (moduleOrRepositoryContext[TReference, TMetadata]) doGetenv(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	var defaultValue *string
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"name", unpack.Bind(thread, &name, unpack.String),
		"default?", unpack.Bind(thread, &defaultValue, unpack.IfNotNone(unpack.Pointer(unpack.String))),
	); err != nil {
		return nil, err
	}

	// TODO: Provide a real implementation!
	if defaultValue == nil {
		return starlark.None, nil
	}
	return starlark.String(*defaultValue), nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doPath(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := mrc.maybeGetStableInputRootPath(); err != nil {
		return nil, err
	}

	var filePath *model_starlark.BarePath
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"path", unpack.Bind(thread, &filePath, mrc.pathUnpackerInto),
	); err != nil {
		return nil, err
	}
	return model_starlark.NewPath(filePath, mrc), nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doRead(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	mrc.maybeGetDirectoryReaders()
	mrc.maybeGetFileReader()
	if err := mrc.maybeGetStableInputRootPath(); err != nil {
		return nil, err
	}
	if mrc.directoryLoadOptions == nil || mrc.fileReader == nil {
		return nil, evaluation.ErrMissingDependency
	}

	if len(args) > 1 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want at most 1", b.Name(), len(args))
	}
	var filePath *model_starlark.BarePath
	var watch string
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"path", unpack.Bind(thread, &filePath, mrc.pathUnpackerInto),
		"watch?", unpack.Bind(thread, &watch, unpack.String),
	); err != nil {
		return nil, err
	}

	r := &changeTrackingDirectoryExistingFileResolver[TReference]{
		loadOptions: mrc.directoryLoadOptions,
		stack:       util.NewNonEmptyStack(mrc.inputRootDirectory),
	}
	if err := path.Resolve(filePath, mrc.virtualRootScopeWalkerFactory.New(r)); err != nil {
		return nil, fmt.Errorf("cannot resolve %#v: %w", filePath.GetUNIXString(), err)
	}

	if r.gotScope {
		// Path resolves to a location inside the input root.
		// Read the file directly.
		patchedFile, err := r.getFile()
		if err != nil {
			return nil, fmt.Errorf("cannot get file %#v: %w", filePath.GetUNIXString(), err)
		}

		f, err := patchedFile.contents.openRead(mrc.context, mrc.fileReader, mrc.patchedFiles)
		if err != nil {
			return nil, fmt.Errorf("failed to open file %#v: %w", filePath.GetUNIXString(), err)
		}

		// TODO: Limit maximum read size!
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %#v: %w", filePath.GetUNIXString(), err)
		}
		return starlark.String(string(data)), nil
	}

	// Path resolves to a location that is not part of the input
	// root (e.g., a file provided by the operating system or stored
	// in the home directory). Invoke "cat" to read the file's
	// contents.
	mrc.maybeGetCommandEncoder()
	mrc.maybeGetDirectoryCreationParametersMessage()
	mrc.maybeGetFileCreationParameters()
	mrc.maybeGetFileCreationParametersMessage()
	mrc.maybeGetRepoPlatform()
	stableInputRootPathError := mrc.maybeGetStableInputRootPath()
	if mrc.commandEncoder == nil ||
		mrc.directoryCreationParametersMessage == nil ||
		mrc.fileCreationParameters == nil ||
		mrc.fileCreationParametersMessage == nil ||
		mrc.fileReader == nil ||
		!mrc.repoPlatform.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	if stableInputRootPathError != nil {
		return nil, stableInputRootPathError
	}

	environment := map[string]string{}
	for _, environmentVariable := range mrc.repoPlatform.Message.RepositoryOsEnviron {
		if _, ok := environment[environmentVariable.Name]; !ok {
			environment[environmentVariable.Name] = environmentVariable.Value
		}
	}
	referenceFormat := mrc.computer.getReferenceFormat()
	environmentVariableList, err := mrc.computer.convertDictToEnvironmentVariableList(environment, mrc.commandEncoder)
	if err != nil {
		return nil, err
	}

	createdCommand, err := model_core.MarshalAndEncodePatchedMessage(
		model_core.NewPatchedMessage(
			&model_command_pb.Command{
				Arguments: []*model_command_pb.ArgumentList_Element{
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: "cat",
						},
					},
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: filePath.GetUNIXString(),
						},
					},
				},
				EnvironmentVariables:        environmentVariableList.Message,
				DirectoryCreationParameters: mrc.directoryCreationParametersMessage,
				FileCreationParameters:      mrc.fileCreationParametersMessage,
				WorkingDirectory:            "/",
				NeedsStableInputRootPath:    true,
			},
			environmentVariableList.Patcher,
		),
		referenceFormat,
		mrc.commandEncoder,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create command: %w", err)
	}

	inputRootReference, err := mrc.computer.createMerkleTreeFromChangeTrackingDirectory(mrc.context, mrc.environment, mrc.inputRootDirectory, mrc.directoryCreationParameters, mrc.directoryReaders, mrc.fileCreationParameters, mrc.patchedFiles)
	if err != nil {
		return nil, fmt.Errorf("failed to create Merkle tree of root directory: %w", err)
	}

	// Execute the command.
	keyPatcher := inputRootReference.Patcher
	actionResult := mrc.environment.GetActionResultValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ActionResult_Key{
				PlatformPkixPublicKey: mrc.repoPlatform.Message.ExecPkixPublicKey,
				CommandReference: keyPatcher.AddReference(
					createdCommand.Contents.GetReference(),
					dag.NewSimpleObjectContentsWalker(createdCommand.Contents, createdCommand.Metadata),
				),
				InputRootReference: inputRootReference.Message.Reference,
				ExecutionTimeout:   &durationpb.Duration{Seconds: 300},
				ExitCodeMustBeZero: true,
			},
			keyPatcher,
		),
	)
	if !actionResult.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}

	// Extract standard output.
	outputs, err := model_parser.MaybeDereference(mrc.context, mrc.directoryReaders.CommandOutputs, model_core.Nested(actionResult, actionResult.Message.OutputsReference))
	if err != nil {
		return nil, fmt.Errorf("failed to obtain outputs from action result: %w", err)
	}

	stdoutEntry, err := model_filesystem.NewFileContentsEntryFromProto(
		model_core.Nested(outputs, outputs.Message.GetStdout()),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid standard output entry: %w", err)
	}
	stdout, err := mrc.fileReader.FileReadAll(mrc.context, stdoutEntry, 1<<22)
	if err != nil {
		return nil, fmt.Errorf("failed to read standard output: %w", err)
	}
	return starlark.String(string(stdout)), nil
}

func (moduleOrRepositoryContext[TReference, TMetadata]) doReportProgress(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var status string
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"status", unpack.Bind(thread, &status, unpack.String),
	); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doWatch(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := mrc.maybeGetStableInputRootPath(); err != nil {
		return nil, err
	}

	var filePath *model_starlark.BarePath
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"path", unpack.Bind(thread, &filePath, mrc.pathUnpackerInto),
	); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) doWhich(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	mrc.maybeGetCommandEncoder()
	mrc.maybeGetDirectoryCreationParameters()
	mrc.maybeGetDirectoryCreationParametersMessage()
	mrc.maybeGetDirectoryReaders()
	mrc.maybeGetFileCreationParametersMessage()
	mrc.maybeGetFileReader()
	mrc.maybeGetRepoPlatform()
	if mrc.commandEncoder == nil ||
		mrc.directoryCreationParameters == nil ||
		mrc.directoryCreationParametersMessage == nil ||
		mrc.directoryReaders == nil ||
		mrc.fileCreationParametersMessage == nil ||
		mrc.fileReader == nil ||
		!mrc.repoPlatform.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}

	var program string
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"program", unpack.Bind(thread, &program, unpack.String),
	); err != nil {
		return nil, err
	}

	environment := map[string]string{}
	for _, environmentVariable := range mrc.repoPlatform.Message.RepositoryOsEnviron {
		environment[environmentVariable.Name] = environmentVariable.Value
	}
	environmentVariableList, err := mrc.computer.convertDictToEnvironmentVariableList(environment, mrc.commandEncoder)
	if err != nil {
		return nil, err
	}

	referenceFormat := mrc.computer.getReferenceFormat()
	createdCommand, err := model_core.MarshalAndEncodePatchedMessage(
		model_core.NewPatchedMessage(
			&model_command_pb.Command{
				Arguments: []*model_command_pb.ArgumentList_Element{
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: "sh",
						},
					},
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: "-c",
						},
					},
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: shellquote.Join("command", "-v", "--", program),
						},
					},
				},
				EnvironmentVariables:        environmentVariableList.Message,
				DirectoryCreationParameters: mrc.directoryCreationParametersMessage,
				FileCreationParameters:      mrc.fileCreationParametersMessage,
				WorkingDirectory:            path.EmptyBuilder.GetUNIXString(),
			},
			environmentVariableList.Patcher,
		),
		referenceFormat,
		mrc.commandEncoder,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create command: %w", err)
	}

	createdInputRoot, err := model_core.MarshalAndEncodePatchedMessage(
		model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](
			&model_filesystem_pb.Directory{
				Leaves: &model_filesystem_pb.Directory_LeavesInline{
					LeavesInline: &model_filesystem_pb.Leaves{},
				},
			},
		),
		referenceFormat,
		mrc.directoryCreationParameters.GetEncoder(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create input root: %w", err)
	}

	// Invoke command.
	keyPatcher := model_core.NewReferenceMessagePatcher[dag.ObjectContentsWalker]()
	actionResult := mrc.environment.GetActionResultValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ActionResult_Key{
				PlatformPkixPublicKey: mrc.repoPlatform.Message.ExecPkixPublicKey,
				CommandReference: keyPatcher.AddReference(
					createdCommand.Contents.GetReference(),
					dag.NewSimpleObjectContentsWalker(createdCommand.Contents, createdCommand.Metadata),
				),
				InputRootReference: keyPatcher.AddReference(
					createdInputRoot.Contents.GetReference(),
					dag.NewSimpleObjectContentsWalker(createdInputRoot.Contents, createdInputRoot.Metadata),
				),
				ExecutionTimeout: &durationpb.Duration{Seconds: 60},
			},
			keyPatcher,
		),
	)
	if !actionResult.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}

	if actionResult.Message.ExitCode == 0 {
		// Capture the standard output of "command -v" and trim the
		// trailing newline character that it adds.
		outputs, err := model_parser.MaybeDereference(mrc.context, mrc.directoryReaders.CommandOutputs, model_core.Nested(actionResult, actionResult.Message.OutputsReference))
		if err != nil {
			return nil, fmt.Errorf("failed to obtain outputs from action result: %w", err)
		}

		stdoutEntry, err := model_filesystem.NewFileContentsEntryFromProto(
			model_core.Nested(outputs, outputs.Message.GetStdout()),
		)
		if err != nil {
			return nil, fmt.Errorf("invalid standard output entry: %w", err)
		}
		stdout, err := mrc.fileReader.FileReadAll(mrc.context, stdoutEntry, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("failed to read standard output: %w", err)
		}
		stdoutStr := strings.TrimSuffix(string(stdout), "\n")
		var resolver model_starlark.PathResolver
		if err := path.Resolve(
			path.UNIXFormat.NewParser(stdoutStr),
			path.NewAbsoluteScopeWalker(&resolver),
		); err != nil {
			return nil, fmt.Errorf("failed to resolve path %#v: %w", stdoutStr, err)
		}
		return model_starlark.NewPath(resolver.CurrentPath, mrc), nil
	} else {
		// A non-zero exit code indicates that the utility could
		// not be found.
		//
		// https://pubs.opengroup.org/onlinepubs/9799919799/utilities/command.html
		return starlark.None, nil
	}
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) Exists(p *model_starlark.BarePath) (bool, error) {
	mrc.maybeGetDirectoryReaders()
	if err := mrc.maybeGetStableInputRootPath(); err != nil {
		return false, err
	}
	if mrc.directoryLoadOptions == nil {
		return false, evaluation.ErrMissingDependency
	}

	r := &changeTrackingDirectoryExistingFileResolver[TReference]{
		loadOptions: mrc.directoryLoadOptions,
		stack:       util.NewNonEmptyStack(mrc.inputRootDirectory),
	}
	if err := path.Resolve(p, mrc.virtualRootScopeWalkerFactory.New(r)); err != nil {
		if errors.Is(err, errDirectoryDoesNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("cannot resolve %#v: %w", p.GetUNIXString(), err)
	}
	if r.gotScope {
		if r.TerminalName == nil {
			// No trailing filename, meaning the path corresponds to
			// the directory that we're in right now.
			return true, nil
		}

		d := r.stack.Peek()
		if _, ok := d.directories[*r.TerminalName]; ok {
			return true, nil
		}
		if _, ok := d.files[*r.TerminalName]; ok {
			return true, nil
		}
		if _, ok := d.symlinks[*r.TerminalName]; ok {
			return true, nil
		}
		return false, nil
	}

	// Path resolves to a location that is not part of the input
	// root (e.g., a file provided by the operating system or stored
	// in the home directory). Invoke "test -e" to check the file's
	// existence.
	mrc.maybeGetCommandEncoder()
	mrc.maybeGetDirectoryCreationParametersMessage()
	mrc.maybeGetFileCreationParameters()
	mrc.maybeGetFileCreationParametersMessage()
	mrc.maybeGetRepoPlatform()
	if mrc.commandEncoder == nil ||
		mrc.directoryCreationParametersMessage == nil ||
		mrc.fileCreationParameters == nil ||
		mrc.fileCreationParametersMessage == nil ||
		!mrc.repoPlatform.IsSet() {
		return false, evaluation.ErrMissingDependency
	}

	environment := map[string]string{}
	for _, environmentVariable := range mrc.repoPlatform.Message.RepositoryOsEnviron {
		if _, ok := environment[environmentVariable.Name]; !ok {
			environment[environmentVariable.Name] = environmentVariable.Value
		}
	}
	referenceFormat := mrc.computer.getReferenceFormat()
	environmentVariableList, err := mrc.computer.convertDictToEnvironmentVariableList(environment, mrc.commandEncoder)
	if err != nil {
		return false, err
	}

	createdCommand, err := model_core.MarshalAndEncodePatchedMessage(
		model_core.NewPatchedMessage(
			&model_command_pb.Command{
				Arguments: []*model_command_pb.ArgumentList_Element{
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: "sh",
						},
					},
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: "-c",
						},
					},
					{
						Level: &model_command_pb.ArgumentList_Element_Leaf{
							Leaf: shellquote.Join("test", "-e", p.GetUNIXString()),
						},
					},
				},
				EnvironmentVariables:        environmentVariableList.Message,
				DirectoryCreationParameters: mrc.directoryCreationParametersMessage,
				FileCreationParameters:      mrc.fileCreationParametersMessage,
				WorkingDirectory:            "/",
				NeedsStableInputRootPath:    true,
			},
			environmentVariableList.Patcher,
		),
		referenceFormat,
		mrc.commandEncoder,
	)
	if err != nil {
		return false, fmt.Errorf("failed to create command: %w", err)
	}

	inputRootReference, err := mrc.computer.createMerkleTreeFromChangeTrackingDirectory(mrc.context, mrc.environment, mrc.inputRootDirectory, mrc.directoryCreationParameters, mrc.directoryReaders, mrc.fileCreationParameters, mrc.patchedFiles)
	if err != nil {
		return false, fmt.Errorf("failed to create Merkle tree of root directory: %w", err)
	}

	// Execute the command.
	keyPatcher := inputRootReference.Patcher
	actionResult := mrc.environment.GetActionResultValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ActionResult_Key{
				PlatformPkixPublicKey: mrc.repoPlatform.Message.ExecPkixPublicKey,
				CommandReference: keyPatcher.AddReference(
					createdCommand.Contents.GetReference(),
					dag.NewSimpleObjectContentsWalker(createdCommand.Contents, createdCommand.Metadata),
				),
				InputRootReference: inputRootReference.Message.Reference,
				ExecutionTimeout:   &durationpb.Duration{Seconds: 300},
			},
			keyPatcher,
		),
	)
	if !actionResult.IsSet() {
		return false, evaluation.ErrMissingDependency
	}
	return actionResult.Message.ExitCode == 0, nil
}

func (moduleOrRepositoryContext[TReference, TMetadata]) IsDir(*model_starlark.BarePath) (bool, error) {
	return false, errors.New("TODO: Implement path.is_dir()!")
}

func (moduleOrRepositoryContext[TReference, TMetadata]) Readdir(*model_starlark.BarePath) ([]path.Component, error) {
	return nil, errors.New("TODO: Implement path.readdir()!")
}

func (moduleOrRepositoryContext[TReference, TMetadata]) Realpath(*model_starlark.BarePath) (*model_starlark.BarePath, error) {
	return nil, errors.New("TODO: Implement path.realpath()!")
}

// externalRepoAddingPathUnpackerInto is a decorator for
// UnpackerInto[*model_starlark.BarePath] that checks whether paths
// refer to ones belonging to external repositories. If they do, it
// ensures that the repo belonging to that path is added to the input
// root.
type externalRepoAddingPathUnpackerInto[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	context      *moduleOrRepositoryContext[TReference, TMetadata]
	base         unpack.UnpackerInto[*model_starlark.BarePath]
	externalPath *model_starlark.BarePath
}

func (ui *externalRepoAddingPathUnpackerInto[TReference, TMetadata]) maybeAddExternalRepo(bp *model_starlark.BarePath) error {
	mrc := ui.context
	if components := bp.GetRelativeTo(ui.externalPath); len(components) >= 1 {
		repoName := components[0]
		if !slices.Equal(mrc.subdirectoryComponents, []path.Component{model_starlark.ComponentExternal, repoName}) {
			// Path belongs to an external repo that is
			// different from the repo that is currently
			// being constructed.
			externalDirectory, err := mrc.inputRootDirectory.getOrCreateDirectory(model_starlark.ComponentExternal)
			if err != nil {
				return fmt.Errorf("Failed to create directory %#v: %w", model_starlark.ComponentStrExternal, err)
			}
			if err := externalDirectory.maybeLoadContents(mrc.directoryLoadOptions); err != nil {
				return fmt.Errorf("failed to load contents of %#v directory: %w", err)
			}

			if _, ok := externalDirectory.directories[repoName]; !ok {
				// External repo does not exist within
				// the input root. Fetch it.
				repo := mrc.environment.GetRepoValue(&model_analysis_pb.Repo_Key{
					CanonicalRepo: repoName.String(),
				})
				if !repo.IsSet() {
					return evaluation.ErrMissingDependency
				}
				repoDirectory, err := externalDirectory.getOrCreateDirectory(repoName)
				if err != nil {
					return fmt.Errorf("failed to create directory for repo: %w", err)
				}
				rootDirectoryReference := repo.Message.RootDirectoryReference
				if rootDirectoryReference == nil {
					return errors.New("root directory reference is not set")
				}
				repoDirectory.currentReference = model_core.Nested(repo, rootDirectoryReference)
			}
		}
	}
	return nil
}

func (ui *externalRepoAddingPathUnpackerInto[TReference, TMetadata]) UnpackInto(thread *starlark.Thread, v starlark.Value, dst **model_starlark.BarePath) error {
	if err := ui.base.UnpackInto(thread, v, dst); err != nil {
		return err
	}
	return ui.maybeAddExternalRepo(*dst)
}

func (ui *externalRepoAddingPathUnpackerInto[TReference, TMetadata]) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	var bp *model_starlark.BarePath
	if err := ui.UnpackInto(thread, v, &bp); err != nil {
		return nil, err
	}
	if err := ui.maybeAddExternalRepo(bp); err != nil {
		return nil, err
	}
	return starlark.String(bp.GetUNIXString()), nil
}

func (ui *externalRepoAddingPathUnpackerInto[TReference, TMetadata]) GetConcatenationOperator() syntax.Token {
	return ui.base.GetConcatenationOperator()
}

// relativizeSymlinks is a post-processing pass that can be applied
// against the repo contents to eliminate any symbolic links that have
// absolute targets, or are relative and for which we can't trivially
// determine they don't escape the repo.
//
// This post-processing is necessary to ensure that the FileProperties
// function can expand symbolic links without depending on the stable
// input root path, or files stored on the host file system of the repo
// platform workers.
func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) relativizeSymlinks(maximumEscapementLevels uint32) error {
	mrc.maybeGetDirectoryCreationParameters()
	if err := mrc.maybeGetStableInputRootPath(); err != nil {
		return err
	}
	if mrc.directoryLoadOptions == nil {
		return evaluation.ErrMissingDependency
	}

	dStack := util.NewNonEmptyStack(mrc.inputRootDirectory)
	var dPath *path.Trace
	for _, component := range mrc.subdirectoryComponents {
		var ok bool
		d, ok := dStack.Peek().directories[component]
		if !ok {
			return nil
		}
		dStack.Push(d)
		dPath = dPath.Append(component)
	}
	return mrc.relativizeSymlinksRecursively(dStack, dPath, maximumEscapementLevels)
}

type changeTrackingDirectoryNormalizingPathResolver[TReference object.BasicReference] struct {
	loadOptions *changeTrackingDirectoryLoadOptions[TReference]

	gotScope    bool
	directories util.NonEmptyStack[*changeTrackingDirectory[TReference]]
	components  []path.Component
}

func (r *changeTrackingDirectoryNormalizingPathResolver[TReference]) OnAbsolute() (path.ComponentWalker, error) {
	r.gotScope = true
	r.directories.PopAll()
	r.components = r.components[:0]
	return r, nil
}

func (r *changeTrackingDirectoryNormalizingPathResolver[TReference]) OnRelative() (path.ComponentWalker, error) {
	r.gotScope = true
	return r, nil
}

func (r *changeTrackingDirectoryNormalizingPathResolver[TReference]) OnDriveLetter(driveLetter rune) (path.ComponentWalker, error) {
	return nil, errors.New("drive letters are not supported")
}

func (r *changeTrackingDirectoryNormalizingPathResolver[TReference]) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := r.directories.Peek()
	if err := d.maybeLoadContents(r.loadOptions); err != nil {
		return nil, err
	}

	if dChild, ok := d.directories[name]; ok {
		r.components = append(r.components, name)
		r.directories.Push(dChild)
		return path.GotDirectory{
			Child:        r,
			IsReversible: true,
		}, nil
	}

	return nil, fmt.Errorf("directory %#v does not exist", name.String())
}

func (r *changeTrackingDirectoryNormalizingPathResolver[TReference]) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	d := r.directories.Peek()
	if err := d.maybeLoadContents(r.loadOptions); err != nil {
		return nil, err
	}
	r.components = append(r.components, name)
	return nil, nil
}

func (r *changeTrackingDirectoryNormalizingPathResolver[TReference]) OnUp() (path.ComponentWalker, error) {
	if _, ok := r.directories.PopSingle(); !ok {
		r.components = r.components[:len(r.components)-1]
		return nil, errors.New("path resolves to a location above the root directory")
	}
	return r, nil
}

func (mrc *moduleOrRepositoryContext[TReference, TMetadata]) relativizeSymlinksRecursively(dStack util.NonEmptyStack[*changeTrackingDirectory[TReference]], dPath *path.Trace, maximumEscapementLevels uint32) error {
	d := dStack.Peek()
	if d.currentReference.IsSet() {
		currentMaximumEscapementLevels := d.currentReference.Message.MaximumSymlinkEscapementLevels
		if currentMaximumEscapementLevels != nil && currentMaximumEscapementLevels.Value <= maximumEscapementLevels {
			// This directory is guaranteed to not contain
			// any symlinks that escape beyond the maximum
			// number of permitted levels. There is no need
			// to traverse it.
			return nil
		}

		if err := d.maybeLoadContents(mrc.directoryLoadOptions); err != nil {
			return err
		}
	}

	for name, target := range d.symlinks {
		escapementCounter := model_filesystem.NewEscapementCountingScopeWalker()
		targetParser := path.UNIXFormat.NewParser(target)
		if err := path.Resolve(targetParser, escapementCounter); err != nil {
			return fmt.Errorf("failed to resolve symlink %#v with target %#v: %w", name.String(), target, err)
		}
		if levels := escapementCounter.GetLevels(); levels == nil || levels.Value > maximumEscapementLevels {
			// Target of this symlink is absolute or has too
			// many ".." components. We need to rewrite it
			// or replace it by its target.
			r := changeTrackingDirectoryNormalizingPathResolver[TReference]{
				loadOptions: mrc.directoryLoadOptions,
				directories: dStack.Copy(),
				components:  append([]path.Component(nil), dPath.ToList()...),
			}
			if err := path.Resolve(targetParser, mrc.virtualRootScopeWalkerFactory.New(&r)); err != nil {
				return fmt.Errorf("failed to resolve symlink %#v with target %#v: %w", dPath.Append(name).GetUNIXString(), target, err)
			}
			if !r.gotScope {
				// TODO: Copy files from the repo worker's host file system.
				return fmt.Errorf("Symlink %#v with target %#v resolves to a path outside the input root", dPath.Append(name).GetUNIXString(), target)
			}

			directoryComponents := dPath.ToList()
			targetComponents := r.components
			matchingCount := 0
			for matchingCount < len(directoryComponents) && matchingCount < len(targetComponents) && directoryComponents[matchingCount] == targetComponents[matchingCount] {
				matchingCount++
			}
			if matchingCount+int(maximumEscapementLevels) < len(directoryComponents) {
				// TODO: Copy files as well?
				return fmt.Errorf("Symlink %#v with target %#v resolves to a path outside the repo", dPath.Append(name).GetUNIXString(), target)
			}

			// Replace the symlink's target with a relative path.
			// TODO: Any way we can cleanly implement this
			// on top of pkg/filesystem/path?
			dotDotsCount := len(directoryComponents) - matchingCount
			parts := make([]string, 0, dotDotsCount+len(targetComponents)-matchingCount)
			for i := 0; i < dotDotsCount; i++ {
				parts = append(parts, "..")
			}
			for _, component := range targetComponents[matchingCount:] {
				parts = append(parts, component.String())
			}
			d.symlinks[name] = strings.Join(parts, "/")
		}
	}

	for name, dChild := range d.directories {
		dStack.Push(dChild)
		if err := mrc.relativizeSymlinksRecursively(dStack, dPath.Append(name), maximumEscapementLevels+1); err != nil {
			return err
		}
		if _, ok := dStack.PopSingle(); !ok {
			panic("should have popped previously pushed directory")
		}
	}
	return nil
}

func (c *baseComputer[TReference, TMetadata]) fetchModuleExtensionRepo(ctx context.Context, canonicalRepo label.CanonicalRepo, apparentRepo label.ApparentRepo, e RepoEnvironment[TReference, TMetadata]) (PatchedRepoValue, error) {
	// Obtain the definition of the declared repo.
	repoValue := e.GetModuleExtensionRepoValue(&model_analysis_pb.ModuleExtensionRepo_Key{
		CanonicalRepo: canonicalRepo.String(),
	})
	if !repoValue.IsSet() {
		return PatchedRepoValue{}, evaluation.ErrMissingDependency
	}
	repo := repoValue.Message.Definition
	if repo == nil {
		return PatchedRepoValue{}, errors.New("no repo definition present")
	}
	return c.fetchRepo(
		ctx,
		canonicalRepo,
		apparentRepo,
		model_core.Nested(repoValue, repo),
		e,
	)
}

func (c *baseComputer[TReference, TMetadata]) fetchRepo(ctx context.Context, canonicalRepo label.CanonicalRepo, apparentRepo label.ApparentRepo, repo model_core.Message[*model_starlark_pb.Repo_Definition, TReference], e RepoEnvironment[TReference, TMetadata]) (PatchedRepoValue, error) {
	// Obtain the definition of the repository rule used by the repo.
	rootModuleValue := e.GetRootModuleValue(&model_analysis_pb.RootModule_Key{})
	allBuiltinsModulesNames := e.GetBuiltinsModuleNamesValue(&model_analysis_pb.BuiltinsModuleNames_Key{})
	repoPlatform := e.GetRegisteredRepoPlatformValue(&model_analysis_pb.RegisteredRepoPlatform_Key{})
	repositoryRule, gotRepositoryRule := e.GetRepositoryRuleObjectValue(&model_analysis_pb.RepositoryRuleObject_Key{
		Identifier: repo.Message.RepositoryRuleIdentifier,
	})
	if !gotRepositoryRule || !allBuiltinsModulesNames.IsSet() || !repoPlatform.IsSet() || !rootModuleValue.IsSet() {
		return PatchedRepoValue{}, evaluation.ErrMissingDependency
	}

	rootModuleName, err := label.NewModule(rootModuleValue.Message.RootModuleName)
	if err != nil {
		return PatchedRepoValue{}, err
	}
	rootPackage := rootModuleName.ToModuleInstance(nil).GetBareCanonicalRepo().GetRootPackage()

	thread := c.newStarlarkThread(ctx, e, allBuiltinsModulesNames.Message.BuiltinsModuleNames)
	attrs := map[string]any{
		"name": starlark.String(canonicalRepo.String()),
	}

	var errIter error
	attrValues := maps.Collect(
		model_starlark.AllStructFields(
			ctx,
			c.valueReaders.List,
			model_core.Nested(repo, repo.Message.AttrValues),
			&errIter,
		),
	)
	if err != nil {
		return PatchedRepoValue{}, err
	}

	for _, publicAttr := range repositoryRule.Attrs.Public {
		if value, ok := attrValues[publicAttr.Name]; ok {
			decodedValue, err := model_starlark.DecodeValue[TReference, TMetadata](
				value,
				/* currentIdentifier = */ nil,
				c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
					return model_starlark.NewLabel[TReference, TMetadata](resolvedLabel), nil
				}),
			)
			if err != nil {
				return PatchedRepoValue{}, err
			}

			// Determine the attribute type, so that the provided value can be canonicalized.
			canonicalizer := publicAttr.AttrType.GetCanonicalizer(rootPackage)
			canonicalizedValue, err := canonicalizer.Canonicalize(thread, decodedValue)
			if err != nil {
				return PatchedRepoValue{}, fmt.Errorf("canonicalize attribute %#v: %w", publicAttr.Name, err)
			}
			attrs[publicAttr.Name] = canonicalizedValue
			delete(attrValues, publicAttr.Name)
		} else if d := publicAttr.Default; d != nil {
			attrs[publicAttr.Name] = d
		} else {
			return PatchedRepoValue{}, fmt.Errorf("missing value for mandatory attribute %#v", publicAttr.Name)
		}
	}
	if len(attrValues) > 0 {
		return PatchedRepoValue{}, fmt.Errorf("unknown attribute %#v", slices.Min(slices.Collect(maps.Keys(attrValues))))
	}

	for name, value := range repositoryRule.Attrs.Private {
		attrs[name] = value
	}

	// Invoke the implementation function.
	subdirectoryComponents := []path.Component{
		model_starlark.ComponentExternal,
		path.MustNewComponent(canonicalRepo.String()),
	}
	repositoryContext, err := c.newModuleOrRepositoryContext(ctx, e, subdirectoryComponents)
	if err != nil {
		return PatchedRepoValue{}, err
	}
	defer repositoryContext.release()

	// These are needed at the end to create the directory Merkle tree.
	repositoryContext.maybeGetDirectoryCreationParameters()
	repositoryContext.maybeGetDirectoryReaders()
	repositoryContext.maybeGetFileCreationParameters()

	repositoryCtx := model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
		// Fields shared with module_ctx.
		"download":             starlark.NewBuiltin("repository_ctx.download", repositoryContext.doDownload),
		"download_and_extract": starlark.NewBuiltin("repository_ctx.download_and_extract", repositoryContext.doDownloadAndExtract),
		"execute":              starlark.NewBuiltin("repository_ctx.execute", repositoryContext.doExecute),
		"extract":              starlark.NewBuiltin("repository_ctx.extract", repositoryContext.doExtract),
		"file":                 starlark.NewBuiltin("repository_ctx.file", repositoryContext.doFile),
		"getenv":               starlark.NewBuiltin("repository_ctx.getenv", repositoryContext.doGetenv),
		"os":                   newRepositoryOS[TReference, TMetadata](thread, repoPlatform.Message),
		"path":                 starlark.NewBuiltin("repository_ctx.path", repositoryContext.doPath),
		"read":                 starlark.NewBuiltin("repository_ctx.read", repositoryContext.doRead),
		"report_progress":      starlark.NewBuiltin("repository_ctx.report_progress", repositoryContext.doReportProgress),
		"watch":                starlark.NewBuiltin("repository_ctx.watch", repositoryContext.doWatch),
		"which":                starlark.NewBuiltin("repository_ctx.which", repositoryContext.doWhich),

		// Fields specific to repository_ctx.
		"attr": model_starlark.NewStructFromDict[TReference, TMetadata](nil, attrs),
		"delete": starlark.NewBuiltin(
			"repository_ctx.delete",
			func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				repositoryContext.maybeGetDirectoryCreationParameters()
				if err := repositoryContext.maybeGetStableInputRootPath(); err != nil {
					return nil, err
				}
				if repositoryContext.directoryLoadOptions == nil {
					return nil, evaluation.ErrMissingDependency
				}

				var filePath *model_starlark.BarePath
				if err := starlark.UnpackArgs(
					b.Name(), args, kwargs,
					"path", unpack.Bind(thread, &filePath, repositoryContext.pathUnpackerInto),
				); err != nil {
					return nil, err
				}

				r := &changeTrackingDirectoryExistingFileResolver[TReference]{
					loadOptions: repositoryContext.directoryLoadOptions,
					stack:       util.NewNonEmptyStack(repositoryContext.inputRootDirectory),
				}
				if err := path.Resolve(filePath, repositoryContext.virtualRootScopeWalkerFactory.New(r)); err != nil {
					if errors.Is(err, errDirectoryDoesNotExist) {
						return starlark.Bool(false), nil
					}
					return nil, fmt.Errorf("cannot resolve %#v: %w", filePath.GetUNIXString(), err)
				}
				if r.TerminalName == nil {
					return nil, fmt.Errorf("%#v does not resolve to a file", filePath.GetUNIXString())
				}

				d := r.stack.Peek()
				if err := d.maybeLoadContents(repositoryContext.directoryLoadOptions); err != nil {
					return nil, err
				}

				if _, ok := d.directories[*r.TerminalName]; ok {
					delete(d.directories, *r.TerminalName)
					return starlark.Bool(true), nil
				}
				if _, ok := d.files[*r.TerminalName]; ok {
					delete(d.files, *r.TerminalName)
					return starlark.Bool(true), nil
				}
				if _, ok := d.symlinks[*r.TerminalName]; ok {
					delete(d.symlinks, *r.TerminalName)
					return starlark.Bool(true), nil
				}
				return starlark.Bool(false), nil
			},
		),
		"name": starlark.String(canonicalRepo.String()),
		"patch": starlark.NewBuiltin(
			"repository_ctx.patch",
			func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				repositoryContext.maybeGetDirectoryCreationParameters()
				if err := repositoryContext.maybeGetStableInputRootPath(); err != nil {
					return nil, err
				}
				if err := repositoryContext.maybeInitializePatchedFiles(); err != nil {
					return nil, err
				}
				if repositoryContext.directoryLoadOptions == nil {
					return nil, evaluation.ErrMissingDependency
				}

				var patchFile *model_starlark.BarePath
				var strip int
				var watchPatch string = "auto"
				if err := starlark.UnpackArgs(
					b.Name(), args, kwargs,
					"patch_file", unpack.Bind(thread, &patchFile, repositoryContext.pathUnpackerInto),
					"strip?", unpack.Bind(thread, &strip, unpack.Int[int]()),
					"watch_patch?", unpack.Bind(thread, &watchPatch, unpack.String),
				); err != nil {
					return nil, err
				}

				// Resolve patch file from the stable root directory.
				r := &changeTrackingDirectoryExistingFileResolver[TReference]{
					loadOptions: repositoryContext.directoryLoadOptions,
					stack:       util.NewNonEmptyStack(repositoryContext.inputRootDirectory),
				}
				if err := path.Resolve(patchFile, repositoryContext.virtualRootScopeWalkerFactory.New(r)); err != nil {
					if errors.Is(err, errDirectoryDoesNotExist) {
						return starlark.Bool(false), nil
					}
					return nil, fmt.Errorf("cannot resolve %#v: %w", patchFile.GetUNIXString(), err)
				}
				trackedPatchFile, err := r.getFile()
				if err != nil {
					return nil, fmt.Errorf("%#v does not resolve to a file: %w", patchFile.GetUNIXString(), err)
				}

				// Resolve patch module directory as the "root" directory.
				repoDirectory, err := repositoryContext.resolveRepoDirectory()
				if err != nil {
					return nil, err
				}

				// Apply the patch to the current repository.
				err = repositoryContext.computer.applyPatch(
					repositoryContext.context,
					repoDirectory,
					repositoryContext.directoryLoadOptions,
					strip,
					repositoryContext.fileReader,
					func() (io.Reader, error) {
						return trackedPatchFile.contents.openRead(repositoryContext.context, repositoryContext.fileReader, repositoryContext.patchedFiles)
					},
					repositoryContext.patchedFiles,
					repositoryContext.patchedFilesWriter,
				)
				if err != nil {
					return nil, fmt.Errorf("cannot apply patch %q: %w", patchFile.GetUNIXString(), err)
				}
				return starlark.None, nil
			},
		),
		"symlink": starlark.NewBuiltin(
			"repository_ctx.symlink",
			func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				repositoryContext.maybeGetDirectoryCreationParameters()
				if err := repositoryContext.maybeGetStableInputRootPath(); err != nil {
					return nil, err
				}
				if repositoryContext.directoryLoadOptions == nil {
					return nil, evaluation.ErrMissingDependency
				}

				var target *model_starlark.BarePath
				var linkName *model_starlark.BarePath
				if err := starlark.UnpackArgs(
					b.Name(), args, kwargs,
					"target", unpack.Bind(thread, &target, repositoryContext.pathUnpackerInto),
					"link_name", unpack.Bind(thread, &linkName, repositoryContext.pathUnpackerInto),
				); err != nil {
					return nil, err
				}

				// Resolve path at which symlink needs
				// to be created.
				r := &changeTrackingDirectoryNewFileResolver[TReference]{
					loadOptions: repositoryContext.directoryLoadOptions,
					stack:       util.NewNonEmptyStack(repositoryContext.inputRootDirectory),
				}
				if err := path.Resolve(linkName, repositoryContext.virtualRootScopeWalkerFactory.New(r)); err != nil {
					return nil, fmt.Errorf("cannot resolve %#v: %w", linkName.GetUNIXString(), err)
				}
				if r.TerminalName == nil {
					return nil, fmt.Errorf("%#v does not resolve to a file", linkName.GetUNIXString())
				}

				// Create symbolic link node.
				d := r.stack.Peek()
				if err := d.maybeLoadContents(repositoryContext.directoryLoadOptions); err != nil {
					return nil, err
				}
				if d.symlinks == nil {
					d.symlinks = map[path.Component]string{}
				}
				d.symlinks[*r.TerminalName] = target.GetUNIXString()
				delete(d.directories, *r.TerminalName)
				delete(d.files, *r.TerminalName)
				return starlark.None, nil
			},
		),
		"template": starlark.NewBuiltin(
			"repository_ctx.template",
			func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				repositoryContext.maybeGetDirectoryCreationParameters()
				repositoryContext.maybeGetFileReader()
				stableInputRootPathError := repositoryContext.maybeGetStableInputRootPath()
				if repositoryContext.directoryCreationParameters == nil || repositoryContext.fileReader == nil {
					return nil, evaluation.ErrMissingDependency
				}
				if stableInputRootPathError != nil {
					return nil, stableInputRootPathError
				}

				if len(args) > 4 {
					return nil, fmt.Errorf("%s: got %d positional arguments, want at most 4", b.Name(), len(args))
				}
				var filePath *model_starlark.BarePath
				var templatePath *model_starlark.BarePath
				var substitutions map[string]string
				executable := true
				var watchTemplate string
				if err := starlark.UnpackArgs(
					b.Name(), args, kwargs,
					"path", unpack.Bind(thread, &filePath, repositoryContext.pathUnpackerInto),
					"template", unpack.Bind(thread, &templatePath, repositoryContext.pathUnpackerInto),
					"substitutions?", unpack.Bind(thread, &substitutions, unpack.Dict(unpack.String, unpack.String)),
					"executable?", unpack.Bind(thread, &executable, unpack.Bool),
					"watch_template?", unpack.Bind(thread, &watchTemplate, unpack.String),
				); err != nil {
					return nil, err
				}

				needles := make([][]byte, 0, len(substitutions))
				replacements := make([][]byte, 0, len(substitutions))
				for _, needle := range slices.Sorted(maps.Keys(substitutions)) {
					needles = append(needles, []byte(needle))
					replacements = append(replacements, []byte(substitutions[needle]))
				}
				searchAndReplacer, err := search.NewMultiSearchAndReplacer(needles)
				if err != nil {
					return nil, fmt.Errorf("invalid substitution keys: %w", err)
				}

				// Load the template file.
				templateFileResolver := &changeTrackingDirectoryExistingFileResolver[TReference]{
					loadOptions: repositoryContext.directoryLoadOptions,
					stack:       util.NewNonEmptyStack(repositoryContext.inputRootDirectory),
				}
				if err := path.Resolve(templatePath, repositoryContext.virtualRootScopeWalkerFactory.New(templateFileResolver)); err != nil {
					return nil, fmt.Errorf("cannot resolve template %#v: %w", templatePath.GetUNIXString(), err)
				}
				f, err := templateFileResolver.getFile()
				if err != nil {
					return nil, fmt.Errorf("cannot get file for template %#v: %w", templatePath.GetUNIXString(), err)
				}
				templateFile, err := f.contents.openRead(
					repositoryContext.context,
					repositoryContext.fileReader,
					repositoryContext.patchedFiles,
				)
				if err != nil {
					return nil, fmt.Errorf("failed to open template %#v: %w", templatePath.GetUNIXString(), err)
				}

				outputFileResolver := &changeTrackingDirectoryNewFileResolver[TReference]{
					loadOptions: repositoryContext.directoryLoadOptions,
					stack:       util.NewNonEmptyStack(repositoryContext.inputRootDirectory),
				}
				if err := path.Resolve(filePath, repositoryContext.virtualRootScopeWalkerFactory.New(outputFileResolver)); err != nil {
					return nil, fmt.Errorf("cannot resolve %#v: %w", filePath.GetUNIXString(), err)
				}
				if outputFileResolver.TerminalName == nil {
					return nil, fmt.Errorf("%#v does not resolve to a file", filePath.GetUNIXString())
				}

				if err := repositoryContext.maybeInitializePatchedFiles(); err != nil {
					return nil, err
				}

				// Perform substitutions.
				patchedFileOffsetBytes := repositoryContext.patchedFilesWriter.GetOffsetBytes()
				if err := searchAndReplacer.SearchAndReplace(repositoryContext.patchedFilesWriter, bufio.NewReader(templateFile), replacements); err != nil {
					return nil, fmt.Errorf("failed to write to file at %#v: %w", filePath.GetUNIXString(), err)
				}

				if err := outputFileResolver.stack.Peek().setFile(
					repositoryContext.directoryLoadOptions,
					*outputFileResolver.TerminalName,
					&changeTrackingFile[TReference]{
						isExecutable: executable,
						contents: patchedFileContents[TReference]{
							offsetBytes: patchedFileOffsetBytes,
							sizeBytes:   repositoryContext.patchedFilesWriter.GetOffsetBytes() - patchedFileOffsetBytes,
						},
					},
				); err != nil {
					return nil, err
				}

				return starlark.None, nil
			},
		),
	})
	repositoryCtx.Freeze()

	_, err = starlark.Call(
		thread,
		repositoryRule.Implementation,
		/* args = */ starlark.Tuple{repositoryCtx},
		/* kwargs = */ nil,
	)
	if err != nil {
		var evalErr *starlark.EvalError
		if !errors.Is(err, evaluation.ErrMissingDependency) && errors.As(err, &evalErr) {
			return PatchedRepoValue{}, errors.New(evalErr.Backtrace())
		}
		return PatchedRepoValue{}, err
	}

	if err := repositoryContext.relativizeSymlinks(1); err != nil {
		return PatchedRepoValue{}, err
	}

	// Capture the resulting external/${repo} directory.
	repoDirectory, err := repositoryContext.resolveRepoDirectory()
	if err != nil {
		return PatchedRepoValue{}, err
	}

	if repositoryContext.directoryCreationParameters == nil ||
		repositoryContext.directoryReaders == nil ||
		repositoryContext.fileCreationParameters == nil {
		return PatchedRepoValue{}, evaluation.ErrMissingDependency
	}
	return c.returnRepoMerkleTree(
		ctx,
		e,
		repoDirectory,
		repositoryContext.directoryCreationParameters,
		repositoryContext.directoryReaders,
		repositoryContext.fileCreationParameters,
		repositoryContext.patchedFiles,
	)
}

func (c *baseComputer[TReference, TMetadata]) createMerkleTreeFromChangeTrackingDirectory(ctx context.Context, e model_core.ExistingObjectCapturer[TReference, TMetadata], rootDirectory *changeTrackingDirectory[TReference], directoryCreationParameters *model_filesystem.DirectoryCreationParameters, directoryReaders *DirectoryReaders[TReference], fileCreationParameters *model_filesystem.FileCreationParameters, patchedFiles io.ReaderAt) (model_core.PatchedMessage[*model_filesystem_pb.DirectoryReference, dag.ObjectContentsWalker], error) {
	if r := rootDirectory.currentReference; r.IsSet() {
		// Directory remained completely unmodified. Simply
		// return the original directory.
		m := model_core.Patch(e, r)
		return model_core.NewPatchedMessage(
			m.Message,
			model_core.MapReferenceMetadataToWalkers(m.Patcher),
		), nil
	}

	// We had to strip a path prefix or apply one or more patches.
	// This means we need to create a new Merkle tree.
	merkleTreeNodes, err := c.filePool.NewFile()
	if err != nil {
		return model_core.PatchedMessage[*model_filesystem_pb.DirectoryReference, dag.ObjectContentsWalker]{}, err
	}
	defer func() {
		if merkleTreeNodes != nil {
			merkleTreeNodes.Close()
		}
	}()

	group, groupCtx := errgroup.WithContext(ctx)
	var createdRootDirectory model_filesystem.CreatedDirectory[model_core.FileBackedObjectLocation]
	fileWritingObjectCapturer := model_core.NewFileWritingObjectCapturer(model_filesystem.NewSectionWriter(merkleTreeNodes))
	group.Go(func() error {
		return model_filesystem.CreateDirectoryMerkleTree(
			groupCtx,
			semaphore.NewWeighted(1),
			group,
			directoryCreationParameters,
			&capturableChangeTrackingDirectory[TReference]{
				options: &capturableChangeTrackingDirectoryOptions[TReference]{
					context:                groupCtx,
					directoryReader:        directoryReaders.Directory,
					fileCreationParameters: fileCreationParameters,
					fileMerkleTreeCapturer: model_filesystem.NewSimpleFileMerkleTreeCapturer(fileWritingObjectCapturer),
					patchedFiles:           patchedFiles,
				},
				directory: rootDirectory,
			},
			model_filesystem.NewSimpleDirectoryMerkleTreeCapturer(fileWritingObjectCapturer),
			&createdRootDirectory,
		)
	})
	if err := group.Wait(); err != nil {
		return model_core.PatchedMessage[*model_filesystem_pb.DirectoryReference, dag.ObjectContentsWalker]{}, err
	}

	// Store the root directory itself. We don't embed it into the
	// response, as that prevents it from being accessed separately.
	createdRootDirectoryObject, err := model_core.MarshalAndEncodePatchedMessage(
		createdRootDirectory.Message,
		c.getReferenceFormat(),
		directoryCreationParameters.GetEncoder(),
	)
	if err != nil {
		return model_core.PatchedMessage[*model_filesystem_pb.DirectoryReference, dag.ObjectContentsWalker]{}, err
	}
	capturedRootDirectory := fileWritingObjectCapturer.CaptureCreatedObject(createdRootDirectoryObject)

	// Finalize writing of Merkle tree nodes to disk, and provide
	// read access to the nodes, so that they can be uploaded.
	if err := fileWritingObjectCapturer.Flush(); err != nil {
		return model_core.PatchedMessage[*model_filesystem_pb.DirectoryReference, dag.ObjectContentsWalker]{}, err
	}
	objectContentsWalkerFactory := model_core.NewFileReadingObjectContentsWalkerFactory(merkleTreeNodes)
	defer objectContentsWalkerFactory.Release()
	merkleTreeNodes = nil

	patcher := model_core.NewReferenceMessagePatcher[dag.ObjectContentsWalker]()
	rootReference := createdRootDirectoryObject.Contents.GetReference()
	return model_core.NewPatchedMessage(
		createdRootDirectory.ToDirectoryReference(
			patcher.AddReference(
				rootReference,
				objectContentsWalkerFactory.CreateObjectContentsWalker(rootReference, capturedRootDirectory),
			),
		),
		patcher,
	), nil
}

func (c *baseComputer[TReference, TMetadata]) returnRepoMerkleTree(ctx context.Context, e model_core.ExistingObjectCapturer[TReference, TMetadata], rootDirectory *changeTrackingDirectory[TReference], directoryCreationParameters *model_filesystem.DirectoryCreationParameters, directoryReaders *DirectoryReaders[TReference], fileCreationParameters *model_filesystem.FileCreationParameters, patchedFiles io.ReaderAt) (PatchedRepoValue, error) {
	rootDirectoryReference, err := c.createMerkleTreeFromChangeTrackingDirectory(ctx, e, rootDirectory, directoryCreationParameters, directoryReaders, fileCreationParameters, patchedFiles)
	if err != nil {
		return PatchedRepoValue{}, err
	}
	return model_core.NewPatchedMessage(
		&model_analysis_pb.Repo_Value{
			RootDirectoryReference: rootDirectoryReference.Message,
		},
		rootDirectoryReference.Patcher,
	), nil
}

func (c *baseComputer[TReference, TMetadata]) ComputeRepoValue(ctx context.Context, key *model_analysis_pb.Repo_Key, e RepoEnvironment[TReference, TMetadata]) (PatchedRepoValue, error) {
	canonicalRepo, err := label.NewCanonicalRepo(key.CanonicalRepo)
	if err != nil {
		return PatchedRepoValue{}, fmt.Errorf("invalid canonical repo: %w", err)
	}

	if _, _, ok := canonicalRepo.GetModuleExtension(); ok {
		return c.fetchModuleExtensionRepo(ctx, canonicalRepo, canonicalRepo.GetModuleInstance().GetModule().ToApparentRepo(), e)
	} else {
		moduleInstance := canonicalRepo.GetModuleInstance()
		if _, ok := moduleInstance.GetModuleVersion(); !ok {
			// See if this is one of the modules for which sources
			// are provided. If so, return a repo value immediately.
			// This allows any files contained within to be accessed
			// without processing MODULE.bazel. This prevents cyclic
			// dependencies.
			buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
			if !buildSpecification.IsSet() {
				return PatchedRepoValue{}, evaluation.ErrMissingDependency
			}

			// Check to see if the client overrode this module manually.
			moduleName := moduleInstance.GetModule().String()
			modules := buildSpecification.Message.BuildSpecification.GetModules()
			if i, ok := sort.Find(
				len(modules),
				func(i int) int { return strings.Compare(moduleName, modules[i].Name) },
			); ok {
				// Found matching module.
				rootDirectoryReference := model_core.Patch(e, model_core.Nested(buildSpecification, modules[i].RootDirectoryReference))
				return model_core.NewPatchedMessage(
					&model_analysis_pb.Repo_Value{
						RootDirectoryReference: rootDirectoryReference.Message,
					},
					model_core.MapReferenceMetadataToWalkers(rootDirectoryReference.Patcher),
				), nil
			}

			// Check to see if there is a MODULE.bazel override for this module.
			var singleVersionOverridePatchLabels, singleVersionOverridePatchCommands []string
			var singleVersionOverridePatchStrip int
			remoteOverridesValue := e.GetModulesWithRemoteOverridesValue(&model_analysis_pb.ModulesWithRemoteOverrides_Key{})
			if !remoteOverridesValue.IsSet() {
				return PatchedRepoValue{}, evaluation.ErrMissingDependency
			}
			remoteOverrides := remoteOverridesValue.Message.ModuleOverrides
			if i := sort.Search(
				len(remoteOverrides),
				func(i int) bool { return remoteOverrides[i].Name >= moduleName },
			); i < len(remoteOverrides) && remoteOverrides[i].Name == moduleName {
				// Found the remote override
				remoteOverride := remoteOverrides[i]
				switch override := remoteOverride.Kind.(type) {
				case *model_analysis_pb.ModuleOverride_RepositoryRule:
					return c.fetchRepo(
						ctx,
						canonicalRepo,
						canonicalRepo.GetModuleInstance().GetModule().ToApparentRepo(),
						model_core.Nested(remoteOverridesValue, override.RepositoryRule),
						e,
					)
				case *model_analysis_pb.ModuleOverride_SingleVersion_:
					if override.SingleVersion.Version != "" {
						return PatchedRepoValue{}, fmt.Errorf("TODO: single version override with exact version should skip Minimal Version Selection!")
					}
					singleVersionOverridePatchLabels = override.SingleVersion.PatchLabels
					singleVersionOverridePatchCommands = override.SingleVersion.PatchCommands
					singleVersionOverridePatchStrip = int(override.SingleVersion.PatchStrip)
				default:
					// TODO: Implement Archive, SingleVersion, MultipleVersions
					return PatchedRepoValue{}, fmt.Errorf("remote override type for %q: %w", remoteOverride.Name, errors.ErrUnsupported)
				}
			}

			// If a version of the module is selected as
			// part of the final build list, we can download
			// that exact version.
			buildListValue := e.GetModuleFinalBuildListValue(&model_analysis_pb.ModuleFinalBuildList_Key{})
			if !buildListValue.IsSet() {
				return PatchedRepoValue{}, evaluation.ErrMissingDependency
			}
			buildList := buildListValue.Message.BuildList
			if i, ok := sort.Find(
				len(buildList),
				func(i int) int { return strings.Compare(moduleName, buildList[i].Name) },
			); ok {
				return c.fetchModuleFromRegistry(
					ctx,
					buildList[i],
					e,
					singleVersionOverridePatchLabels,
					singleVersionOverridePatchCommands,
					singleVersionOverridePatchStrip,
				)
			}
		}
	}

	return PatchedRepoValue{}, errors.New("repo not found")
}
