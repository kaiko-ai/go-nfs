package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// onLink implements the NFSv3 LINK RPC (hard link). RFC 1813 LINK3args is
//
//	struct LINK3args { nfs_fh3 file; diropargs3 link; }
//
// i.e. the handle of the existing file to link to, followed by the directory
// handle + name where the new link is created. The backing filesystem must
// implement UnixChange (hard links); SFTP exposes this via the
// hardlink@openssh.com extension.
func onLink(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter

	fileHandle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	link := DirOpArg{}
	if err := xdr.Read(w.req.Body, &link); err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	// Existing file to link to.
	fs, filePath, err := userHandle.FromHandle(fileHandle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	// Directory the new link is created in.
	dirFS, dirPath, err := userHandle.FromHandle(link.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	if !billy.CapabilityCheck(dirFS, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}
	if len(string(link.Filename)) > PathNameMax {
		return &NFSStatusError{NFSStatusNameTooLong, os.ErrInvalid}
	}

	existingPath := fs.Join(filePath...)
	newFilePath := dirFS.Join(append(dirPath, string(link.Filename))...)
	if _, err := dirFS.Stat(newFilePath); err == nil {
		return &NFSStatusError{NFSStatusExist, os.ErrExist}
	}
	if s, err := dirFS.Stat(dirFS.Join(dirPath...)); err != nil {
		return &NFSStatusError{NFSStatusAccess, err}
	} else if !s.IsDir() {
		return &NFSStatusError{NFSStatusNotDir, nil}
	}

	changer := userHandle.Change(dirFS)
	if changer == nil {
		return &NFSStatusError{NFSStatusNotSupp, os.ErrInvalid}
	}
	cos, ok := changer.(UnixChange)
	if !ok {
		return &NFSStatusError{NFSStatusNotSupp, os.ErrInvalid}
	}
	if err := cos.Link(existingPath, newFilePath); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	// LINK3resok { post_op_attr file_attributes; wcc_data linkdir_wcc; }
	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, filePath)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WriteWcc(writer, nil, tryStat(dirFS, dirPath)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
