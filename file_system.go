package main

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ledongthuc/pdf"
)

func getContent(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".txt", ".go", ".py", ".md", ".json", ".js", ".ts":
		content, err := os.ReadFile(path)
		return string(content), err
	case ".pdf":
		return readPDF(path)
	case ".docx":
		return readDocx(path)
	default:
		return "", fmt.Errorf("format nesuportat: %s", ext)
	}
}
func readPDF(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	b, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	buf.ReadFrom(b)
	return buf.String(), nil
}

func readDocx(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	var textBuilder strings.Builder
	for _, f := range r.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			decoder := xml.NewDecoder(rc)
			for {
				token, _ := decoder.Token()
				if token == nil {
					break
				}
				switch se := token.(type) {
				case xml.StartElement:
					if se.Name.Local == "t" {
						var text string
						decoder.DecodeElement(&text, &se)
						textBuilder.WriteString(text)
					}
				case xml.EndElement:
					if se.Name.Local == "p" {
						textBuilder.WriteString("\n")
					}
				}
			}
			break
		}
	}
	return textBuilder.String(), nil
}

const cacheFileName = ".autodev_cache.json"

func computeHash(content []byte) string {
	hash := md5.Sum(content)
	return hex.EncodeToString(hash[:])
}

func getProjectFiles(dir string) string {
	var tree strings.Builder
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && (strings.HasPrefix(info.Name(), ".") || info.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !info.IsDir() {
			relPath, _ := filepath.Rel(dir, path)
			if relPath != cacheFileName && !strings.HasPrefix(info.Name(), "autodev") {
				tree.WriteString("- " + relPath + "\n")
			}
		}
		return nil
	})
	if tree.Len() == 0 {
		return "Niciun fișier găsit.\n"
	}
	return tree.String()
}
func isFileProtected(path string) bool {
	// Listează căile sau directoarele care NU trebuie atinse niciodată
	protectedPaths := []string{
		"AppData",
		"site-packages",
		"Lib\\site-packages",
		"Python313",
		"autodev_config.json",
		".autodev_history.json",
		".autodev_cache.json",
	}
	for _, p := range protectedPaths {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}
