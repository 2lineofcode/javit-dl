package downloader

type KeyInfo struct {
	Method string
	URI    string
	IV     []byte
}

type Segment struct {
	URI      string
	Seq      int64
	Filename string
}
