package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Set an upload limit of 1 GB (1 << 30 bytes) using http.MaxBytesReader.
	const maxMemory = 10 << 30 // 1 GB
	http.MaxBytesReader(w, r.Body, maxMemory)

	// Extract the videoID from the URL path parameters and parse it as a UUID
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate the user to get a userID
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

	// Get the dbVideo metadata from the database, if the user is not the dbVideo owner, return a http.StatusUnauthorized response
	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't get video", err)
		return
	}
	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video not owned by user", err)
		return
	}

	// Parse the uploaded video file from the form data
	fmt.Println("uploading video for video", videoID, "by user", userID)
	videoFile, videoHeaders, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video file from form", err)
		return
	}
	defer videoFile.Close()

	// Validate the uploaded file to ensure it's an MP4 video
	mediaType, _, err := mime.ParseMediaType(videoHeaders.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", err)
		return
	}
	fileExt := "mp4"

	// Save the uploaded file to a temporary file on disk.
	tmpFile, err := os.CreateTemp("", "tubely-video-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp dir", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()
	_, err = io.Copy(tmpFile, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", err)
		return
	}

	// Reset the tempFile's file pointer to the beginning with .Seek(0, io.SeekStart) - this will allow us to read the file again from the beginning
	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek file", err)
		return
	}

	// Determine video aspect ratio using ffprobe
	aspectRatio, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}

	// Process the video for fast start using ffmpeg
	processedFilePath, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath)

	// Reopen the processed file
	tmpFile, err = os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video file", err)
		return
	}
	defer tmpFile.Close()

	// Put the object into S3 using PutObject.
	key := make([]byte, 32)
	rand.Read(key)
	var objName string
	switch aspectRatio {
	case "16:9":
		objName = fmt.Sprintf("landscape/%s.%s", base64.RawURLEncoding.EncodeToString(key), fileExt)
	case "9:16":
		objName = fmt.Sprintf("portrait/%s.%s", base64.RawURLEncoding.EncodeToString(key), fileExt)
	default:
		objName = fmt.Sprintf("other/%s.%s", base64.RawURLEncoding.EncodeToString(key), fileExt)
	}

	// Use the S3 client to upload the file
	fmt.Printf("Uploading video to S3 bucket %s with key %s\n", cfg.s3Bucket, objName)
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &objName,
		ContentType: &mediaType,
		Body:        tmpFile,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload to S3", err)
		return
	}

	// Update the VideoURL of the video record in the database with the S3 bucket and key. S3 URLs are in the format https://<bucket-name>.s3.<region>.amazonaws.com/<key>.
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, objName)
	dbVideo.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video URL in database", err)
		return
	}

	respondWithJSON(w, http.StatusOK, dbVideo)
}

func getVideoAspectRatio(filePath string) (string, error) {
	// Use ffprobe to get video dimensions
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var b bytes.Buffer
	cmd.Stdout = &b
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	// Parse the JSON output to extract width and height
	type VideoFormat struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	videoFormat := &VideoFormat{}
	err = json.Unmarshal(b.Bytes(), videoFormat)
	if err != nil {
		return "", err
	}
	if len(videoFormat.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}
	width := videoFormat.Streams[0].Width
	height := videoFormat.Streams[0].Height

	// Calculate aspect ratio
	aspectRatio := float32(width) / float32(height)
	if 1.77 < aspectRatio && aspectRatio < 1.78 {
		return "16:9", nil
	} else if 0.56 < aspectRatio && aspectRatio < 0.57 {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

// processes the video file at filePath to enable fast start using ffmpeg.
func processVideoForFastStart(filePath string) (string, error) {
	outputFilepath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilepath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFilepath, nil
}
