// A REST api for the artifact store, implemented using Martini.
//
// Each "Handle" function acts as a handler for a request and is
// routed with Martini (routing is hanlded elsewhere).
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/dropbox/changes-artifacts/database"
	"github.com/dropbox/changes-artifacts/model"
	"github.com/go-martini/martini"
	"github.com/martini-contrib/render"
	"gopkg.in/amz.v1/s3"
)

const DEFAULT_DEADLINE = 30

type CreateArtifactReq struct {
	Name         string
	Chunked      bool
	Size         int64
	DeadlineMins uint
}

func CreateArtifact(req CreateArtifactReq, bucket *model.Bucket, db database.Database) (*model.Artifact, error) {
	if len(req.Name) == 0 {
		return nil, fmt.Errorf("Artifact Name not provided, state = %s", bucket.State)
	}

	if bucket.State != model.OPEN {
		return nil, fmt.Errorf("Bucket is already closed")
	}

	artifact, err := db.GetArtifactByName(bucket.Id, req.Name);
	if err == nil {
		return nil, fmt.Errorf("Artifact already exists")
	}

	artifact = new(model.Artifact)
	artifact.Name = req.Name
	artifact.BucketId = bucket.Id
	artifact.DateCreated = time.Now()

	if req.DeadlineMins == 0 {
		artifact.DeadlineMins = DEFAULT_DEADLINE
	} else {
		artifact.DeadlineMins = req.DeadlineMins
	}

	if req.Chunked {
		artifact.State = model.APPENDING
	} else {
		if req.Size == 0 {
			return nil, fmt.Errorf("Cannot create a new upload artifact without size.")
		}
		artifact.Size = req.Size
		artifact.State = model.WAITING_FOR_UPLOAD
	}
	artifact.Name = req.Name
	if err := db.InsertArtifact(artifact); err != nil {
		return nil, fmt.Errorf("Error inserting artifact %s", err)
	}

	return artifact, nil
}

func HandleCreateArtifact(r render.Render, req *http.Request, db database.Database, params martini.Params, bucket *model.Bucket) {
	if bucket == nil {
		JsonErrorf(r, http.StatusBadRequest,"Error: no bucket specified")
		return
	}

	var createArtifactReq CreateArtifactReq

	err := json.NewDecoder(req.Body).Decode(&createArtifactReq)
	if err != nil {
		JsonErrorf(r, http.StatusBadRequest, "Error decoding json: %s", err.Error())
	}
	fmt.Printf("Artifact creation request: %v\n", createArtifactReq)
	artifact, err := CreateArtifact(createArtifactReq, bucket, db)

	if err != nil {
		JsonErrorf(r, http.StatusInternalServerError, err.Error())
		return
	}

	r.JSON(http.StatusOK, artifact)
}

func ListArtifacts(r render.Render, req *http.Request, db database.Database, params martini.Params, bucket *model.Bucket) {
	if bucket == nil {
		JsonErrorf(r, http.StatusBadRequest, "Error: no bucket specified")
		return
	}

	artifacts, err := db.ListArtifactsInBucket(bucket.Id)
	if err != nil {
		JsonErrorf(r, http.StatusInternalServerError, "Error while listing artifacts: %s", err.Error())
		return
	}

	r.JSON(http.StatusOK, artifacts)
}

func HandleGetArtifact(r render.Render, artifact *model.Artifact) {
	if artifact == nil {
		JsonErrorf(r, http.StatusBadRequest, "Error: no bucket specified")
		return
	}

	r.JSON(http.StatusOK, artifact)
}

func AppendLogChunk(db database.Database, artifact *model.Artifact, logChunk *model.LogChunk) *HttpError {
	if artifact.State != model.APPENDING {
		return NewHttpError(http.StatusBadRequest, fmt.Sprintf("Unexpected artifact state: %s", artifact.State))
	}

	if logChunk.Size <= 0 {
		return NewHttpError(http.StatusBadRequest, "Invalid chunk size %d", logChunk.Size)
	}

	if logChunk.Content == "" {
		return NewHttpError(http.StatusBadRequest, "Empty content string")
	}

	if int64(len(logChunk.Content)) != logChunk.Size {
		return NewHttpError(http.StatusBadRequest, "Content length does not match indicated size")
	}

	// Find previous chunk in DB - append only
	if nextByteOffset, err := db.GetLastByteSeenForArtifact(artifact.Id); err != nil {
		return NewHttpError(http.StatusInternalServerError, "Error while checking for previous byte range: %s", err)
	} else if nextByteOffset != logChunk.ByteOffset {
		return NewHttpError(http.StatusBadRequest, "Overlapping ranges detected, expected offset: %d, actual offset: %d", nextByteOffset, logChunk.ByteOffset)
	}

	logChunk.ArtifactId = artifact.Id

	// Expand artifact size - redundant after above change.
	if artifact.Size < logChunk.ByteOffset+logChunk.Size {
		artifact.Size = logChunk.ByteOffset + logChunk.Size
		if err := db.UpdateArtifact(artifact); err != nil {
			return NewHttpError(http.StatusInternalServerError, err.Error())
		}
	}

	if err := db.InsertLogChunk(logChunk); err != nil {
		return NewHttpError(http.StatusBadRequest, "Error updating log chunk: %s", err)
	}
	return nil
}

func PostArtifact(r render.Render, req *http.Request, db database.Database, s3bucket *s3.Bucket, artifact *model.Artifact) {
	if artifact == nil {
		JsonErrorf(r, http.StatusBadRequest, "Error: no artifact specified")
		return
	}

	switch artifact.State {
	case model.WAITING_FOR_UPLOAD:
		contentLengthStr := req.Header.Get("Content-Length")
		contentLength, err := strconv.ParseInt(contentLengthStr, 10, 64) // string, base, bits
		if err != nil {
			JsonErrorf(r, http.StatusBadRequest, "Error: couldn't parse Content-Length as int64")
		} else if contentLength != artifact.Size {
			JsonErrorf(r, http.StatusBadRequest, "Error: Content-Length does not match artifact size")
		} else if err = PutArtifact(artifact, db, s3bucket, PutArtifactReq{ContentLength: contentLengthStr, Body: req.Body}); err != nil {
			JsonErrorf(r, http.StatusInternalServerError, err.Error())
		} else {
			r.JSON(http.StatusOK, artifact)
		}
		return

	case model.UPLOADING:
		JsonErrorf(r, http.StatusBadRequest, "Error: artifact is currently being updated")
		return

	case model.UPLOADED:
		JsonErrorf(r, http.StatusBadRequest, "Error: artifact already uploaded")
		return

	case model.APPENDING:
		// TODO: Treat contents as a JSON request containing a chunk.
		logChunk := new(model.LogChunk)
		if err := json.NewDecoder(req.Body).Decode(logChunk); err != nil {
			JsonErrorf(r, http.StatusBadRequest, "Error: could not decode JSON request")
			return
		}

		if err := AppendLogChunk(db, artifact, logChunk); err != nil {
			r.JSON(err.errCode, map[string]string{"error": err.Error()})
			return
		}

		r.JSON(http.StatusOK, artifact)
		return

	case model.APPEND_COMPLETE:
		JsonErrorf(r, http.StatusBadRequest, "Error: artifact is closed for further appends")
		return
	}
}

func CloseArtifact(artifact *model.Artifact, db database.Database, s3bucket *s3.Bucket, failIfAlreadyClosed bool) error {
	switch artifact.State {
	case model.UPLOADED:
		// Already closed. Nothing to do here.
		fallthrough
	case model.APPEND_COMPLETE:
		// This artifact will be eventually shipped to S3. No change required.
		return nil

	case model.APPENDING:
		artifact.State = model.APPEND_COMPLETE
		if err := db.UpdateArtifact(artifact); err != nil {
			return err
		}

		return MergeLogChunks(artifact, db, s3bucket)

	case model.WAITING_FOR_UPLOAD:
		// Streaming artifact was not uploaded
		artifact.State = model.CLOSED_WITHOUT_DATA
		if err := db.UpdateArtifact(artifact); err != nil {
			return err
		}

		return nil

	default:
		return fmt.Errorf("Unexpected artifact state: %s", artifact.State)
	}
}

// Merges all of the individual chunks into a single object and stores it on s3.
// The log chunks are stored in the database, while the object is uploaded to s3.
//
// XXX shouldn't we garbage collect the log chunks after the object is on s3?
func MergeLogChunks(artifact *model.Artifact, db database.Database, s3bucket *s3.Bucket) error {
	switch artifact.State {
	case model.APPEND_COMPLETE:
		// TODO: Reimplement using GorpDatabase
		// If the file is empty, don't bother creating an object on S3.
		if artifact.Size == 0 {
			artifact.State = model.CLOSED_WITHOUT_DATA
			artifact.S3URL = ""

			// Conversion between *DatabaseEror and error is tricky. If we don't do this, a nil
			// *DatabaseError can become a non-nil error.
			return db.UpdateArtifact(artifact).GetError()
		}

		// XXX Do we need to commit here or is this handled transparently?
		artifact.State = model.UPLOADING
		if err := db.UpdateArtifact(artifact); err != nil {
			return err
		}

		logChunks, err := db.ListLogChunksInArtifact(artifact.Id)
		if err != nil {
			return err
		}

		r, w := io.Pipe()
		errChan := make(chan error)
		uploadCompleteChan := make(chan bool)
		fileName := artifact.DefaultS3URL()

		// Asynchronously upload the object to s3 while reading from the r, w
		// pipe. Thus anything written to "w" will be sent to S3.
		go func() {
			defer close(errChan)
			defer close(uploadCompleteChan)
			defer r.Close()
			if err := s3bucket.PutReader(fileName, r, artifact.Size, "binary/octet-stream", s3.PublicRead); err != nil {
				errChan <- fmt.Errorf("Error uploading to S3: %s", err)
				return
			}

			uploadCompleteChan <- true
		}()

		for _, logChunk := range logChunks {
			w.Write([]byte(logChunk.Content))
		}

		w.Close()

		// Wait either for S3 upload to complete or for it to fail with an error.
		// XXX This is a long operation and should probably be asynchronous from the
		// actual HTTP request, and the client should poll to check when its uploaded.
		select {
		case _ = <-uploadCompleteChan:
			artifact.State = model.UPLOADED
			artifact.S3URL = fileName
			if err := db.UpdateArtifact(artifact); err != nil {
				return err
			}
			return nil
		case err := <-errChan:
			return err
		}

	case model.WAITING_FOR_UPLOAD:
		fallthrough
	case model.ERROR:
		fallthrough
	case model.APPENDING:
		fallthrough
	case model.UPLOADED:
		fallthrough
	case model.UPLOADING:
		return fmt.Errorf("Artifact can only be merged when in APPEND_COMPLETE state, but state is %s", artifact.State)
	default:
		return fmt.Errorf("Illegal artifact state! State code is %d", artifact.State)
	}
}

func FinalizeArtifact(r render.Render, params martini.Params, db database.Database, s3bucket *s3.Bucket, artifact *model.Artifact) {
	if artifact == nil {
		JsonErrorf(r, http.StatusBadRequest, "Error: no artifact specified")
		return
	}

	if err := CloseArtifact(artifact, db, s3bucket, true); err != nil {
		r.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%s", err)})
		return
	}

	r.JSON(http.StatusOK, map[string]interface{}{})
}

func GetArtifactContent(r render.Render, req *http.Request, res http.ResponseWriter, db database.Database, params martini.Params, s3bucket *s3.Bucket, artifact *model.Artifact) {
	if artifact == nil {
		JsonErrorf(r, http.StatusBadRequest, "Error: no artifact specified")
		return
	}

	switch artifact.State {
	case model.UPLOADED:
		// Fetch from S3
		reader, err := s3bucket.GetReader(artifact.S3URL)
		if err != nil {
			JsonErrorf(r, http.StatusInternalServerError, err.Error())
			return
		}
		// Ideally, we'll use a Hijacker to take over the conn so that we can employ an io.Writer
		// instead of loading the entire file into memory before writing it back out. But, for now, we
		// will run the risk of OOM if large files need to be served.
		var buf bytes.Buffer
		_, err = buf.ReadFrom(reader)
		if err != nil {
			JsonErrorf(r, http.StatusInternalServerError, "Error reading upload buffer: %s", err.Error())
			return
		}
		res.Write(buf.Bytes())
		return
	case model.UPLOADING:
		// Not done uploading to S3 yet. Error.
		r.JSON(http.StatusNotFound, map[string]string{"error": "Waiting for content to complete uploading"})
		return
	case model.APPENDING:
		fallthrough
	case model.APPEND_COMPLETE:
		// Pick from log chunks
		logChunks, err := db.ListLogChunksInArtifact(artifact.Id)
		if err != nil {
			JsonErrorf(r, http.StatusInternalServerError, err.Error())
			return
		}
		var buf bytes.Buffer
		for _, logChunk := range logChunks {
			buf.WriteString(logChunk.Content)
		}
		res.Write(buf.Bytes())
		return
	case model.WAITING_FOR_UPLOAD:
		// Not started yet. Error
		JsonErrorf(r, http.StatusNotFound, "Waiting for content to get uploaded")
		return
	}
}

type PutArtifactReq struct {
	ContentLength string
	Body          io.Reader
}

func PutArtifact(artifact *model.Artifact, db database.Database, bucket *s3.Bucket, req PutArtifactReq) error {
	if artifact.State != model.WAITING_FOR_UPLOAD {
		return fmt.Errorf("Expected artifact to be in state WAITING_FOR_UPLOAD: %s", artifact.State)
	}

	// New file being inserted into DB.
	// Mark status change to UPLOADING and start uploading to S3.
	//
	// First, verify that the size of the content being uploaded matches our expected size.
	var fileSize int64
	var err error

	if req.ContentLength != "" {
		fileSize, err = strconv.ParseInt(req.ContentLength, 10, 64) // string, base, bits
		// This should never happen if a sane HTTP client is used. Nonetheless ...
		if err != nil {
			return fmt.Errorf("Invalid Content-Length specified")
		}
	} else {
		// This too should never happen if a sane HTTP client is used. Nonetheless ...
		return fmt.Errorf("Content-Length not specified")
	}

	if fileSize != artifact.Size {
		return fmt.Errorf("Content length %d does not match expected file size %d", fileSize, artifact.Size)
	}

	// XXX Do we need to commit here or is this handled transparently?
	artifact.State = model.UPLOADING
	if err := db.UpdateArtifact(artifact); err != nil {
		return err
	}

	cleanupAndReturn := func(err error) error {
		// TODO: Is there a better way to detect and handle errors?
		// Use a channel to signify upload completion. In defer, check if the channel is empty. If
		// yes, mark error. Else ignore.
		if err != nil {
			// TODO: s/ERROR/WAITING_FOR_UPLOAD/ ?
			log.Printf("Error uploading to S3: %s\n", err)
			artifact.State = model.ERROR
			err2 := db.UpdateArtifact(artifact);
			if err2 != nil {
				log.Printf("Error while handling error: %s", err2.Error())
			}
			return err
		}

		return nil
	}

	fileName := artifact.DefaultS3URL()
	if err := bucket.PutReader(fileName, req.Body, artifact.Size, "binary/octet-stream", s3.PublicRead); err != nil {
		return cleanupAndReturn(fmt.Errorf("Error uploading to S3: %s", err))
	}

	artifact.State = model.UPLOADED
	artifact.S3URL = fileName
	if err := db.UpdateArtifact(artifact); err != nil {
		return err
	}
	return nil
}

// Returns nil on error.
//
// TODO return errors on error
func GetArtifact(bucket *model.Bucket, artifact_name string, db database.Database) *model.Artifact {
	if bucket == nil {
		return nil
	}

	if artifact, err := db.GetArtifactByName(bucket.Id, artifact_name); err != nil {
		return nil
	} else {
		return artifact
	}
}