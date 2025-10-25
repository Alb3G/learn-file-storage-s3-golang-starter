package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Video for specified user not found", err)
		return
	}

	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are trying to upload a video that isn't yours", err)
		return
	}

	const maxMemmory = 10 << 20

	err = r.ParseMultipartForm(maxMemmory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	imgData, _, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Wrong form key provided", err)
		return
	}
	defer imgData.Close()

	bytes, err := io.ReadAll(imgData)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Wrong file format provided", err)
		return
	}

	mediaType := http.DetectContentType(bytes)
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		errMsg := "images are the only content type allowed"
		respondWithError(w, http.StatusBadRequest, "Uploaded file isn't an image", errors.New(errMsg))
		return
	}
	fileExtension, err := mime.ExtensionsByType(mediaType)
	if err != nil || len(fileExtension) == 0 {
		respondWithError(w, http.StatusBadRequest, err.Error(), err)
		return
	}

	bytesArr := make([]byte, 32)
	if _, err = rand.Read(bytesArr); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong", err)
		return
	}

	encodeStrForFileName := base64.RawURLEncoding.EncodeToString(bytesArr)
	fileName := fmt.Sprintf("%v%v", encodeStrForFileName, fileExtension[0])
	assetsFolderPath := filepath.Join(cfg.assetsRoot, fileName)

	file, err := os.Create(assetsFolderPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}
	defer file.Close()

	_, err = file.Write(bytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying file in disk", err)
		return
	}

	thumbnailUrl := fmt.Sprintf("http://localhost:%v/%v", cfg.port, assetsFolderPath)
	fmt.Println(thumbnailUrl)
	dbVideo.ThumbnailURL = &thumbnailUrl

	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong updating video data", err)
		return
	}

	respondWithJSON(w, http.StatusOK, dbVideo)
}
