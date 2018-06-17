package download_controller

import (
	"context"
	"errors"
	"io"
	"mime"
	"strconv"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
	"github.com/turt2live/matrix-media-repo/common"
	"github.com/turt2live/matrix-media-repo/common/config"
	"github.com/turt2live/matrix-media-repo/controllers/upload_controller"
	"github.com/turt2live/matrix-media-repo/matrix"
	"github.com/turt2live/matrix-media-repo/types"
	"github.com/turt2live/matrix-media-repo/util/resource_handler"
)

type mediaResourceHandler struct {
	resourceHandler *resource_handler.ResourceHandler
}

type downloadRequest struct {
	origin  string
	mediaId string
}

type downloadResponse struct {
	media *types.Media
	err   error
}

type downloadedMedia struct {
	Contents        io.ReadCloser
	DesiredFilename string
	ContentType     string
}

var resHandler *mediaResourceHandler
var resHandlerLock = &sync.Once{}
var downloadErrorsCache *cache.Cache
var downloadErrorCacheSingletonLock = &sync.Once{}

func getResourceHandler() (*mediaResourceHandler) {
	if resHandler == nil {
		resHandlerLock.Do(func() {
			handler, err := resource_handler.New(config.Get().Downloads.NumWorkers, downloadResourceWorkFn)
			if err != nil {
				panic(err)
			}

			resHandler = &mediaResourceHandler{handler}
		})
	}

	return resHandler
}

func (h *mediaResourceHandler) DownloadRemoteMedia(origin string, mediaId string) chan *downloadResponse {
	resultChan := make(chan *downloadResponse)
	go func() {
		reqId := "remote_download:" + origin + "_" + mediaId
		result := <-h.resourceHandler.GetResource(reqId, &downloadRequest{origin, mediaId})
		resultChan <- result.(*downloadResponse)
	}()
	return resultChan
}

func downloadResourceWorkFn(request *resource_handler.WorkRequest) interface{} {
	info := request.Metadata.(*downloadRequest)
	log := logrus.WithFields(logrus.Fields{
		"worker_requestId":      request.Id,
		"worker_requestOrigin":  info.origin,
		"worker_requestMediaId": info.mediaId,
	})
	log.Info("Downloading remote media")

	ctx := context.TODO() // TODO: Should we use a real context?

	downloaded, err := DownloadRemoteMediaDirect(info.origin, info.mediaId, log)
	if err != nil {
		return &downloadResponse{err: err}
	}

	defer downloaded.Contents.Close()

	userId := upload_controller.NoApplicableUploadUser
	media, err := upload_controller.StoreDirect(downloaded.Contents, downloaded.ContentType, downloaded.DesiredFilename, userId, info.origin, info.mediaId, nil, ctx, log)
	if err != nil {
		return &downloadResponse{err: err}
	}

	return &downloadResponse{media, err}
}

func DownloadRemoteMediaDirect(server string, mediaId string, log *logrus.Entry) (*downloadedMedia, error) {
	if downloadErrorsCache == nil {
		downloadErrorCacheSingletonLock.Do(func() {
			cacheTime := time.Duration(config.Get().Downloads.FailureCacheMinutes) * time.Minute
			downloadErrorsCache = cache.New(cacheTime, cacheTime*2)
		})
	}

	cacheKey := server + "/" + mediaId
	item, found := downloadErrorsCache.Get(cacheKey)
	if found {
		log.Warn("Returning cached error for remote media download failure")
		return nil, item.(error)
	}

	baseUrl, err := matrix.GetServerApiUrl(server)
	if err != nil {
		downloadErrorsCache.Set(cacheKey, err, cache.DefaultExpiration)
		return nil, err
	}

	downloadUrl := baseUrl + "/_matrix/media/v1/download/" + server + "/" + mediaId + "?allow_remote=false"
	resp, err := matrix.FederatedGet(downloadUrl, server)
	if err != nil {
		downloadErrorsCache.Set(cacheKey, err, cache.DefaultExpiration)
		return nil, err
	}

	if resp.StatusCode == 404 {
		log.Info("Remote media not found")

		err = common.ErrMediaNotFound
		downloadErrorsCache.Set(cacheKey, err, cache.DefaultExpiration)
		return nil, err
	} else if resp.StatusCode != 200 {
		log.Info("Unknown error fetching remote media; received status code " + strconv.Itoa(resp.StatusCode))

		err = errors.New("could not fetch remote media")
		downloadErrorsCache.Set(cacheKey, err, cache.DefaultExpiration)
		return nil, err
	}

	var contentLength int64 = 0
	if resp.Header.Get("Content-Length") != "" {
		contentLength, err = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			return nil, err
		}
	} else {
		log.Warn("Missing Content-Length header on response - continuing anyway")
	}

	if contentLength > 0 && config.Get().Downloads.MaxSizeBytes > 0 && contentLength > config.Get().Downloads.MaxSizeBytes {
		log.Warn("Attempted to download media that was too large")

		err = common.ErrMediaTooLarge
		downloadErrorsCache.Set(cacheKey, err, cache.DefaultExpiration)
		return nil, err
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		log.Warn("Remote media has no content type; Assuming application/octet-stream")
		contentType = "application/octet-stream" // binary
	}

	request := &downloadedMedia{
		ContentType: contentType,
		Contents:    resp.Body,
		// DesiredFilename (calculated below)
	}

	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err == nil && params["filename"] != "" {
		request.DesiredFilename = params["filename"]
	}

	log.Info("Persisting downloaded media")
	return request, nil
}