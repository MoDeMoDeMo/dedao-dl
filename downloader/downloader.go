package downloader

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/yann0917/dedao-dl/request"
	"github.com/yann0917/dedao-dl/utils"
)

func progressBar(size int, prefix string) *pb.ProgressBar {
	bar := pb.New(size).
		SetMaxWidth(1000).
		SetRefreshRate(time.Millisecond * 10)
	return bar
}

//Download download data
func Download(v Datum, stream string, path string) error {
	//按大到小排序
	v.genSortedStreams()

	title := utils.FileName(v.Title, "")
	if stream == "" {
		stream = v.sortedStreams[0].name
	}
	data, ok := v.Streams[stream]
	if !ok {
		return fmt.Errorf("指定要下载的类型不存在：%s", stream)
	}

	//判断下载连接是否存在
	if len(data.URLs) == 0 {
		return nil
	}

	filePreName := filepath.Join(path, title)
	fileName, err := utils.FilePath(filePreName, "mp3", false)

	if err != nil {
		return err
	}

	if v.Type == "audio" {
		fmt.Println(fileName)
		err = downloadAudio(v.M3U8URL, fileName)
		fmt.Println(err)
		return err
	}
	_, mergedFileExists, err := utils.FileSize(fileName)
	if err != nil {
		return err
	}

	// After the merge, the file size has changed, so we do not check whether the size matches
	if mergedFileExists {
		// fmt.Printf("%s: file already exists, skipping\n", mergedFilePath)
		return nil
	}

	bar := progressBar(data.Size, title)
	bar.Start()

	chunkSizeMB := 1

	if len(data.URLs) == 1 {
		err := Save(data.URLs[0], filePreName, bar, chunkSizeMB)
		if err != nil {
			return err
		}
		bar.Finish()
		return nil
	}

	wgp := utils.NewWaitGroupPool(30)

	errs := make([]error, 0)
	lock := sync.Mutex{}
	parts := make([]string, len(data.URLs))

	for index, url := range data.URLs {
		if len(errs) > 0 {
			break
		}

		partFileName := fmt.Sprintf("%s[%d]", filePreName, index)
		partFilePath, err := utils.FilePath(partFileName, url.Ext, false)
		if err != nil {
			return err
		}
		parts[index] = partFilePath

		wgp.Add()
		go func(url URL, fileName string, bar *pb.ProgressBar) {
			defer wgp.Done()
			err := Save(url, fileName, bar, chunkSizeMB)
			if err != nil {
				lock.Lock()
				errs = append(errs, err)
				lock.Unlock()
			}
		}(url, partFileName, bar)
	}

	wgp.Wait()

	if len(errs) > 0 {
		return errs[0]
	}

	bar.Finish()
	switch v.Type {
	case "audio":
		err = utils.MergeAudio(parts, fileName)
	case "vedio":
		err = utils.MergeAudioAndVideo(parts, fileName)
	}

	if v.Type != "audio" && v.Type != "vedio" {
		return nil
	}

	return err
}

func downloadAudio(m3u8 string, fname string) (err error) {
	err = utils.MergeAudio([]string{m3u8}, fname)
	return
}

// Save save url file
func Save(
	urlData URL, fileName string, bar *pb.ProgressBar, chunkSizeMB int,
) error {
	if urlData.Size == 0 {
		size, err := request.Size(urlData.URL)
		if err != nil {
			return err
		}
		urlData.Size = size
	}

	var err error
	filePath, err := utils.FilePath(fileName, urlData.Ext, false)
	if err != nil {
		return err
	}
	fileSize, exists, err := utils.FileSize(filePath)
	if err != nil {
		return err
	}
	if bar == nil {
		bar = progressBar(urlData.Size, fileName)
		bar.Start()
	}
	// Skip segment file
	// TODO: Live video URLs will not return the size
	if exists && fileSize == urlData.Size {
		bar.Add(fileSize)
		return nil
	}
	tempFilePath := filePath + ".download"
	tempFileSize, _, err := utils.FileSize(tempFilePath)

	if err != nil {
		return err
	}
	headers := map[string]string{}
	var (
		file      *os.File
		fileError error
	)
	if tempFileSize > 0 {
		// range start from 0, 0-1023 means the first 1024 bytes of the file
		headers["Range"] = fmt.Sprintf("bytes=%d-", tempFileSize)
		file, fileError = os.OpenFile(tempFilePath, os.O_APPEND|os.O_WRONLY, 0644)
		bar.Add(tempFileSize)
	} else {
		file, fileError = os.Create(tempFilePath)
	}
	if fileError != nil {
		return fileError
	}

	// close and rename temp file at the end of this function
	defer func() {
		// must close the file before rename or it will cause
		// `The process cannot access the file because it is being used by another process.` error.
		file.Close()
		if err == nil {
			os.Rename(tempFilePath, filePath)
		}
	}()

	if chunkSizeMB > 0 {
		var start, end, chunkSize int
		chunkSize = chunkSizeMB * 1024 * 1024
		remainingSize := urlData.Size
		if tempFileSize > 0 {
			start = tempFileSize
			remainingSize -= tempFileSize
		}
		chunk := remainingSize / chunkSize
		if remainingSize%chunkSize != 0 {
			chunk++
		}
		var i = 1
		for ; i <= chunk; i++ {
			end = start + chunkSize - 1
			headers["Range"] = fmt.Sprintf("bytes=%d-%d", start, end)
			temp := start
			for i := 0; ; i++ {
				written, err := writeFile(urlData.URL, file, headers, bar)
				if err == nil {
					break
				} else if i+1 >= 3 {
					return err
				}
				temp += written
				headers["Range"] = fmt.Sprintf("bytes=%d-%d", temp, end)
				time.Sleep(1 * time.Second)
			}
			start = end + 1
		}
	} else {
		temp := tempFileSize
		for i := 0; ; i++ {
			written, err := writeFile(urlData.URL, file, headers, bar)
			if err == nil {
				break
			} else if i+1 >= 3 {
				return err
			}
			temp += written
			headers["Range"] = fmt.Sprintf("bytes=%d-", temp)
			time.Sleep(1 * time.Second)
		}
	}

	return nil
}

func writeFile(
	url string, file *os.File, headers map[string]string, bar *pb.ProgressBar,
) (int, error) {
	res, err := request.Get(url)
	if err != nil {
		return 0, err
	}
	defer res.Close()

	writer := io.MultiWriter(file)
	bar.SetWriter(writer)
	// Note that io.Copy reads 32kb(maximum) from input and writes them to output, then repeats.
	// So don't worry about memory.
	written, copyErr := io.Copy(writer, res)
	if copyErr != nil && copyErr != io.EOF {
		return int(written), fmt.Errorf("file copy error: %s", copyErr)
	}
	return int(written), nil
}

//PrintToPDF print to pdf
func PrintToPDF(v Datum, cookies map[string]string, path string) error {

	name := utils.FileName(v.Title, "pdf")

	filename := filepath.Join(path, name)
	fmt.Printf("正在生成文件：【\033[37;1m%s\033[0m】 ", name)

	_, exist, err := utils.FileSize(filename)

	if err != nil {
		fmt.Printf("\033[31;1m%s\033[0m\n", "失败")
		return err
	}

	if exist {
		fmt.Printf("\033[33;1m%s\033[0m\n", "已存在")
		return nil
	}

	err = utils.ColumnPrintToPDF(v.Enid, filename, cookies)

	if err != nil {
		fmt.Printf("\033[31;1m%s\033[0m\n", "失败")
		return err
	}

	fmt.Printf("\033[32;1m%s\033[0m\n", "完成")

	return nil
}