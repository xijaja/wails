package assetserver

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/wailsapp/wails/v2/internal/fs"
	"github.com/wailsapp/wails/v2/internal/logger"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed defaultindex.html
var defaultHTML []byte

const (
	indexHTML = "index.html"
)

type assetHandler struct {
	fs      iofs.FS
	handler http.Handler

	logger *logger.Logger

	retryMissingFiles bool
}

func NewAssetHandler(ctx context.Context, options assetserver.Options) (http.Handler, error) {
	var log *logger.Logger
	if _logger := ctx.Value("logger"); _logger != nil {
		log = _logger.(*logger.Logger)
	}

	vfs := options.Assets
	if vfs != nil {
		if _, err := vfs.Open("."); err != nil {
			return nil, err
		}

		subDir, err := fs.FindPathToFile(vfs, indexHTML)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				msg := "no `index.html` could be found in your Assets fs.FS"
				if embedFs, isEmbedFs := vfs.(embed.FS); isEmbedFs {
					rootFolder, _ := fs.FindEmbedRootPath(embedFs)
					msg += fmt.Sprintf(", please make sure the embedded directory '%s' is correct and contains your assets", rootFolder)
				}

				return nil, fmt.Errorf(msg)
			}

			return nil, err
		}

		vfs, err = iofs.Sub(vfs, path.Clean(subDir))
		if err != nil {
			return nil, err
		}
	}

	var result http.Handler = &assetHandler{
		fs:      vfs,
		handler: options.Handler,
		logger:  log,
	}

	if middleware := options.Middleware; middleware != nil {
		result = middleware(result)
	}

	return result, nil
}

func (d *assetHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	url := req.URL.Path
	handler := d.handler
	if strings.EqualFold(req.Method, http.MethodGet) {
		filename := path.Clean(strings.TrimPrefix(url, "/"))

		d.logDebug("Handling request '%s' (file='%s')", url, filename)
		if err := d.serveFSFile(rw, req, filename); err != nil {
			if os.IsNotExist(err) {
				if handler != nil {
					d.logDebug("File '%s' not found, serving '%s' by AssetHandler", filename, url)
					handler.ServeHTTP(rw, req)
					err = nil
				} else {
					rw.WriteHeader(http.StatusNotFound)
					err = nil
				}
			}

			if err != nil {
				d.logError("Unable to load file '%s': %s", filename, err)
				http.Error(rw, err.Error(), http.StatusInternalServerError)
			}
		}
	} else if handler != nil {
		d.logDebug("No GET request, serving '%s' by AssetHandler", url)
		handler.ServeHTTP(rw, req)
	} else {
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// serveFile will try to load the file from the fs.FS and write it to the response
func (d *assetHandler) serveFSFile(rw http.ResponseWriter, req *http.Request, filename string) error {
	if d.fs == nil {
		return os.ErrNotExist
	}

	file, err := d.fs.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	statInfo, err := file.Stat()
	if err != nil {
		return err
	}

	if statInfo.IsDir() {
		url := req.URL.Path
		if url != "" && url[len(url)-1] != '/' {
			// If the URL doesn't end in a slash normally a http.redirect should be done, but that currently doesn't work on
			// WebKit WebVies (macOS/Linux).
			// So we handle this as a file that could not be found.
			return os.ErrNotExist
		}

		filename = path.Join(filename, indexHTML)

		file, err = d.fs.Open(filename)
		if err != nil {
			return err
		}
		defer file.Close()

		statInfo, err = file.Stat()
		if err != nil {
			return err
		}
	}

	var buf [512]byte
	n, err := file.Read(buf[:])
	if err != nil && err != io.EOF {
		return err
	}

	// Detect MimeType by sniffing the first 512 bytes
	// Do the custom MimeType sniffing even though http.ServeContent would do it in case
	// of an io.ReadSeeker. We would like to have a consistent behaviour in both cases.
	if contentType := GetMimetype(filename, buf[:n]); contentType != "" {
		rw.Header().Set(HeaderContentType, contentType)
	}

	if fileSeeker, _ := file.(io.ReadSeeker); fileSeeker != nil {
		if _, err := fileSeeker.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seeker can't seek")
		}

		http.ServeContent(rw, req, statInfo.Name(), statInfo.ModTime(), fileSeeker)
		return nil
	}

	rw.Header().Set(HeaderContentLength, fmt.Sprintf("%d", statInfo.Size()))

	// Write the first 512 bytes used for MimeType sniffing
	_, err = io.Copy(rw, bytes.NewReader(buf[:n]))
	if err != nil {
		return err
	}

	// Copy the remaining content of the file
	_, err = io.Copy(rw, file)
	return err
}

func (d *assetHandler) logDebug(message string, args ...interface{}) {
	if d.logger != nil {
		d.logger.Debug("[AssetHandler] "+message, args...)
	}
}

func (d *assetHandler) logError(message string, args ...interface{}) {
	if d.logger != nil {
		d.logger.Error("[AssetHandler] "+message, args...)
	}
}
