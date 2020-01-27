package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"k8s.io/klog"
)

type credsResponse struct {
	BucketName   string `json:"bucketName"`
	SecretKey    string `json:"secretKey"`
	AccessKey    string `json:"accessKey"`
	SessionToken string `json:"sessionToken"`
	Region       string `json:"region"`
	Key          string `json:"key"`
}

func (c *credsResponse) toAWSCredentials() *credentials.Credentials {
	return credentials.NewStaticCredentials(c.AccessKey, c.SecretKey, c.SessionToken)
}

func (c *credsResponse) createSession() (*session.Session, error) {
	return session.NewSession(&aws.Config{
		Region:      aws.String(c.Region),
		Credentials: c.toAWSCredentials(),
	})
}

func (c *credsResponse) uploadFile(f *os.File) (*s3manager.UploadOutput, error) {
	s, err := c.createSession()
	if err != nil {
		return nil, err
	}

	return uploadFileToS3(s, c, f)
}

// func (c *credsResponse) downloadFile(f *os.File) (int64, error) {
// 	s, err := c.createSession()
// 	if err != nil {
// 		return 0, err
// 	}

// 	return downloadFileFromS3(s, c, f)
// }

func requestCreds() (*credsResponse, error) {
	hydraURL := os.Getenv("HYDRA_URL")
	// hydraAuth := os.Getenv("HYDRA_AUTH")
	hydraUsername := os.Getenv("HYDRA_USER")
	hydraPassword := os.Getenv("HYDRA_PASS")

	insecureClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	// reqData := url.Values{
	// 	"fileName":  []string{fileName},
	// 	"isPrivate": []string{"false"},
	// }
	// reqDataEncoded := reqData.Encode()
	// reqDataReader := strings.NewReader(reqDataEncoded)
	reqDataReader := strings.NewReader(`{ "fileName":"TestFile", "isPrivate": "false" }`)

	req, err := http.NewRequest("POST", hydraURL, reqDataReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	// req.Header.Set("Authorization", hydraAuth)
	req.SetBasicAuth(hydraUsername, hydraPassword)

	resp, err := insecureClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Unexpected HTTP response status code: %s", resp.Status)
	}

	creds := &credsResponse{}
	err = json.NewDecoder(resp.Body).Decode(creds)
	if err != nil {
		return nil, err
	}

	return creds, nil
}

func uploadFileToS3(s *session.Session, creds *credsResponse, file *os.File) (*s3manager.UploadOutput, error) {
	return s3manager.NewUploader(s).Upload(&s3manager.UploadInput{
		Bucket: aws.String(creds.BucketName),
		Key:    aws.String(creds.Key),
		Body:   file,
	})
}

// func downloadFileFromS3(s *session.Session, creds *credsResponse, file *os.File) (int64, error) {
// 	return s3manager.NewDownloader(s).Download(file, &s3.GetObjectInput{
// 		Bucket: aws.String(creds.BucketName),
// 		Key:    aws.String(creds.Key),
// 	})
// }

func dirToTar(dirPath string, rawWriter io.Writer) error {
	// Open the directory.
	dir, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	defer dir.Close()

	// Create a gzip writer into the raw writer (most likely a file or a buffer).
	gzipWriter := gzip.NewWriter(rawWriter)
	defer gzipWriter.Close()
	// Create a tar writer into the gzip writer.
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	err = filepath.Walk(dirPath, func(fullPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories.
		if info.IsDir() {
			return nil
		}

		// Get relative path of file within the directory.
		relPath, err := filepath.Rel(dirPath, fullPath)
		if err != nil {
			return nil
		}

		// Open the file for reading.
		file, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		defer file.Close()

		// Create the tar header.
		header := &tar.Header{
			Name:    relPath,
			Size:    info.Size(),
			Mode:    int64(info.Mode()),
			ModTime: info.ModTime(),
		}

		// Write the tar header.
		err = tarWriter.WriteHeader(header)
		if err != nil {
			return err
		}

		// Copy the file contents into the tar.
		_, err = io.Copy(tarWriter, file)
		if err != nil {
			return err
		}

		return nil
	})

	return err
}

func main() {
	const srcDir = "./must-gather/"
	const tmpTar = "./must-gather.tar.gz"

	klog.Infoln("Creating a temporary archive file...")
	f, err := os.Create(tmpTar)
	if err != nil {
		klog.Fatalln("Unable to create temporary archive file --", err)
	} else {
		klog.Infoln("Temporary archive file created")
	}
	defer f.Close()

	klog.Infoln("Archiving the Must-Gather directory into the temporary file...")
	err = dirToTar(srcDir, f)
	if err != nil {
		klog.Fatalln("Unable to archive Must-Gather directory --", err)
	} else {
		klog.Infoln("Must-Gather directory archived")
	}

	klog.Infoln("Requesting AWS S3 credentials from Hydra...")
	creds, err := requestCreds()
	if err != nil {
		klog.Fatalln("Credentials request failed --", err)
	} else {
		klog.Infoln("S3 credentials received")
	}

	klog.Infoln("Rewinding the temporary archive file...")
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		klog.Fatalln("Unable to rewind archive file --", err)
	} else {
		klog.Infoln("Archive file rewinded")
	}

	klog.Infoln("Uploading Must-Gather archive...")
	_, err = creds.uploadFile(f)
	if err != nil {
		klog.Fatalln("Could not upload file --", err)
	} else {
		klog.Infoln("Must-Gather archive uploaded")
	}

	// -------------------------------------------------

	// fileOut, err := os.Create("./main_copy.go")
	// defer fileOut.Close()
	// if err != nil {
	// 	log.Fatalln("Could not open output file --", err)
	// }

	// _, err = creds.downloadFile(fileOut)
	// if err != nil {
	// 	log.Fatalln("Could not download file --", err)
	// } else {
	// 	log.Println("File downloaded successfully")
	// }
}
