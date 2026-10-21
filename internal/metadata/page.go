package metadata

const PageByteLimit int64 = 1 << 20

// PageEnvelopeBytes reserves the items wrapper, cursor, separators and trailing newline.
const PageEnvelopeBytes int64 = 128

// Six bytes per UTF-8 byte covers JSON control/HTML escaping and U+2028/U+2029.
// The fixed allowance covers field names, generated UUID, revision, timestamps
// and a separator. Current field limits keep every valid record below 128 KiB.
func MaintainerEncodedBound(record Maintainer) int64 {
	return 6*MaintainerFieldBytes(record.MaintainerFields) + 512
}

func ApplicationEncodedBound(record Application) int64 {
	return 6*ApplicationFieldBytes(record.ApplicationFields, record.MaintainerID) + 512
}

func PageItemFits(used, itemBytes int64) bool {
	return used >= PageEnvelopeBytes && itemBytes >= 0 &&
		itemBytes <= PageByteLimit && used <= PageByteLimit-itemBytes
}
