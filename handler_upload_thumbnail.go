package main

import (
	"fmt"
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

	// TODO: implement the upload here
	const maxMemory = 10 << 20 // 10 MB
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse multipart form", err)
		return
	}

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get thumbnail file from form", err)
		return
	}
	defer file.Close()

	// Get the media type
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse media type", err)
		return
	}

	// Read the file data
	fileData := make([]byte, header.Size)
	_, err = file.Read(fileData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't read thumbnail file", err)
		return
	}

	// Verify that the video exists and belongs to the user
	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}
	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't have permission to upload thumbnail for this video", nil)
		return
	}

	// Save the thumbnail file locally
	fileExt := mediaTypeToFileExt(mediaType)
	if fileExt == "" {
		respondWithError(w, http.StatusBadRequest, "Unsupported media type", nil)
		return
	}
	err = saveFileLocally(cfg.assetsRoot, fmt.Sprintf("%s.%s", videoID, fileExt), fileData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save thumbnail file", err)
		return
	}

	// Update video thumbnail URL pointing to local assets
	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s.%s", cfg.port, videoID, fileExt)
	dbVideo.ThumbnailURL = &thumbnailURL
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video with thumbnail URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, dbVideo)
}

func mediaTypeToFileExt(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	default:
		return ""
	}
}

func saveFileLocally(dir, filename string, data []byte) error {
	filePath := filepath.Join(dir, filename)
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(data)
	return err
}
