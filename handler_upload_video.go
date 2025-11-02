package main

import (
	"context"
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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
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

func getVideoAspectRatio(filePath string) (string, error) {
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

func processVideoForFastStart(filePath string) (string, error) {
	// Crear una ruta DIFERENTE para el output
	outputPath := filePath + ".processing" // O cualquier otro sufijo

	cmd := exec.Command("ffmpeg",
		"-i", filePath, // Input: archivo original
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		outputPath, // Output: archivo nuevo
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffmpeg failed: %v\nOutput: %s", err, string(output))
	}

	return outputPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)

	presignResult, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %v", err)
	}

	return presignResult.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	videoUrlParts := strings.Split(*video.VideoURL, ",")
	if len(videoUrlParts) < 2 {
		return video, nil
	}

	bucket := videoUrlParts[0]
	key := videoUrlParts[1]

	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 15*time.Minute)
	if err != nil {
		return video, err
	}

	video.VideoURL = &presignedURL

	return video, nil
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

	_, err = tempFile.Write(bytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Something went wrong while copying files content", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}

	videoAspectRatio, err := getVideoAspectRatio(tempFile.Name())
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

	tempFile.Close() //Close video before processing

	fastStartVideoPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}
	defer os.Remove(fastStartVideoPath)

	fastStartVideoFile, err := os.Open(fastStartVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}
	defer fastStartVideoFile.Close()

	s3Input := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(fileKey),
		Body:        fastStartVideoFile,
		ContentType: &mediaType,
	}

	_, err = cfg.s3Client.PutObject(r.Context(), s3Input)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}

	s3BucketObjUrl := fmt.Sprintf("%v,%v", cfg.s3Bucket, fileKey)

	dbVideo.VideoURL = &s3BucketObjUrl
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}

	video, err := cfg.dbVideoToSignedVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
