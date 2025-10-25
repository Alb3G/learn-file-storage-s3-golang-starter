package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	http.MaxBytesReader(w, r.Body, 1<<30)

	videoIdString := r.PathValue("videoID")
	videoId, err := uuid.Parse(videoIdString)
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

	dbVideo, err := cfg.db.GetVideo(videoId)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Video for specified user not found", err)
		return
	}

	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video for specified user not found", err)
		return
	}

	videoFile, _, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Wrong form key provided", err)
		return
	}
	defer videoFile.Close()

	bytes, err := io.ReadAll(videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error reading video file", err)
		return
	}

	mediaType := http.DetectContentType(bytes)
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "File must contain an extension type of video/mp4", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong while creating a temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())

	err = os.WriteFile(tempFile.Name(), bytes, os.ModeAppend)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong while copying files content", err)
		return
	}
	tempFile.Seek(0, io.SeekStart)

	bytesArr := make([]byte, 32)
	if _, err = rand.Read(bytesArr); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong while creating file name", err)
		return
	}

	fileExtensions, err := mime.ExtensionsByType(mediaType)
	if err != nil || len(fileExtensions) == 0 {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong while parsing file's extension type", err)
		return
	}
	fileKey := base64.RawURLEncoding.EncodeToString(bytesArr) + fileExtensions[1]

	s3Input := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        tempFile,
		ContentType: &mediaType,
	}

	s3BucketObjUrl := fmt.Sprintf("https://%v.s3.%v.amazonaws.com/%v", cfg.s3Bucket, cfg.s3Region, fileKey)

	dbVideo.VideoURL = &s3BucketObjUrl
	cfg.db.UpdateVideo(dbVideo)

	cfg.s3Client.PutObject(r.Context(), s3Input)
}
