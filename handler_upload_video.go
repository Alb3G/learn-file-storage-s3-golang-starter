package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type VideoSpecs struct {
	Streams []struct {
		AspectRatio string `json:"display_aspect_ratio"`
	} `json:"streams"`
}

var aspectRatio map[string]string = map[string]string{
	"16:9": "landscape/",
	"9:16": "portrait/",
}

/*
Steps:
 1. Ensure the file exists
 2. Ensure the mime of the file video/mp4, mv4 etc.. has to be a video
 3. Execute following command: ffprobe -v error -print_format json -show_streams filePath
 4. Unmarshal bytes in a struct to get width and height to determine aspect ratio
 5. Return strings [16:9, 9:16, other]
*/
func getVideoAspectRation(filePath string) (string, error) {
	fileInfo, err := os.Stat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if fileInfo.IsDir() {
		return "", fmt.Errorf("file path pointing to a directory instead of a video file")
	}

	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("empty file provided")
	}

	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}

	bytes := make([]byte, 512)
	_, err = file.Read(bytes)
	if err != nil {
		return "", err
	}

	fileMimeType := http.DetectContentType(bytes)
	if fileMimeType != "video/mp4" {
		return "", fmt.Errorf("mime type of file must be video/mp4")
	}

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams", filePath,
	)

	output, err := cmd.CombinedOutput() // Runs the command
	if err != nil {
		return "", fmt.Errorf("ffprobe failed: %w, output: %s", err, string(output))
	}

	var videoSpecs VideoSpecs
	if err := json.Unmarshal(output, &videoSpecs); err != nil {
		return "", fmt.Errorf("invalid ffprobe output: %w", err)
	}

	if len(videoSpecs.Streams) == 0 {
		return "", fmt.Errorf("no streams found in file")
	}

	return videoSpecs.Streams[0].AspectRatio, nil
}

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
	defer tempFile.Close()

	err = os.WriteFile(tempFile.Name(), bytes, os.ModeAppend)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong while copying files content", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}

	videoAspectRatio, err := getVideoAspectRation(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}

	videoFolderByAspectRatio, exists := aspectRatio[videoAspectRatio]
	if !exists {
		videoFolderByAspectRatio = "other"
	}

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
	fileKey := videoFolderByAspectRatio + base64.RawURLEncoding.EncodeToString(bytesArr) + fileExtensions[1]

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
