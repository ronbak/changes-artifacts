package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"golang.org/x/net/context"

	"github.com/dropbox/changes-artifacts/common"
	"github.com/dropbox/changes-artifacts/database"
	"github.com/dropbox/changes-artifacts/model"
	"github.com/martini-contrib/render"
	"gopkg.in/amz.v1/s3"
)

type HttpError struct {
	errCode int
	errStr  string
}

func (he *HttpError) Error() string {
	return he.errStr
}

func NewHttpError(code int, format string, args ...interface{}) *HttpError {
	if len(args) > 0 {
		return &HttpError{errCode: code, errStr: fmt.Sprintf(format, args...)}
	}
	return &HttpError{errCode: code, errStr: format}
}

func NewWrappedHttpError(code int, err error) *HttpError {
	return &HttpError{errCode: code, errStr: err.Error()}
}

// Ensure that HttpError implements error
var _ error = new(HttpError)

func ListBuckets(ctx context.Context, r render.Render, db database.Database) {
	if buckets, err := db.ListBuckets(); err != nil {
		LogAndRespondWithError(ctx, r, http.StatusBadRequest, err)
	} else {
		r.JSON(http.StatusOK, buckets)
	}
}

func CreateBucket(db database.Database, clk common.Clock, bucketId string, owner string) (*model.Bucket, *HttpError) {
	if bucketId == "" {
		return nil, NewHttpError(http.StatusBadRequest, "Bucket ID not provided")
	}

	if len(owner) == 0 {
		return nil, NewHttpError(http.StatusBadRequest, "Bucket Owner not provided")
	}

	_, err := db.GetBucket(bucketId)
	if err != nil && !err.EntityNotFound() {
		return nil, NewWrappedHttpError(http.StatusInternalServerError, err)
	}
	if err == nil {
		return nil, NewHttpError(http.StatusBadRequest, "Entity exists")
	}

	var bucket model.Bucket
	bucket.Id = bucketId
	bucket.DateCreated = clk.Now()
	bucket.State = model.OPEN
	bucket.Owner = owner
	if err := db.InsertBucket(&bucket); err != nil {
		return nil, NewWrappedHttpError(http.StatusBadRequest, err)
	}
	return &bucket, nil
}

func HandleCreateBucket(ctx context.Context, r render.Render, req *http.Request, db database.Database, clk common.Clock) {
	var createBucketReq struct {
		ID    string
		Owner string
	}

	if err := json.NewDecoder(req.Body).Decode(&createBucketReq); err != nil {
		LogAndRespondWithErrorf(ctx, r, http.StatusBadRequest, "Malformed JSON request")
		return
	}

	if bucket, err := CreateBucket(db, clk, createBucketReq.ID, createBucketReq.Owner); err != nil {
		LogAndRespondWithError(ctx, r, err.errCode, err)
	} else {
		r.JSON(http.StatusOK, bucket)
	}
}

func HandleGetBucket(ctx context.Context, r render.Render, bucket *model.Bucket) {
	if bucket == nil {
		LogAndRespondWithErrorf(ctx, r, http.StatusBadRequest, "No bucket specified")
		return
	}

	r.JSON(http.StatusOK, bucket)
}

// HandleCloseBucket handles the HTTP request to close a bucket. See CloseBucket for details.
func HandleCloseBucket(ctx context.Context, r render.Render, db database.Database, bucket *model.Bucket, s3Bucket *s3.Bucket, clk common.Clock) {
	if bucket == nil {
		LogAndRespondWithErrorf(ctx, r, http.StatusBadRequest, "No bucket specified")
		return
	}

	if err := CloseBucket(ctx, bucket, db, s3Bucket, clk); err != nil {
		LogAndRespondWithError(ctx, r, http.StatusBadRequest, err)
	} else {
		r.JSON(http.StatusOK, bucket)
	}
	return
}

// CloseBucket closes a bucket, preventing further updates. All artifacts associated with the bucket
// are also marked closed. If the bucket is already closed, an error is returned.
func CloseBucket(ctx context.Context, bucket *model.Bucket, db database.Database, s3Bucket *s3.Bucket, clk common.Clock) error {
	if bucket.State != model.OPEN {
		return fmt.Errorf("Bucket is already closed")
	}

	bucket.State = model.CLOSED
	bucket.DateClosed = clk.Now()
	if err := db.UpdateBucket(bucket); err != nil {
		return err
	}

	if artifacts, err := db.ListArtifactsInBucket(bucket.Id); err != nil {
		return err
	} else {
		for _, artifact := range artifacts {
			if err := CloseArtifact(ctx, &artifact, db, s3Bucket, false); err != nil {
				return err
			}
		}
	}

	return nil
}
