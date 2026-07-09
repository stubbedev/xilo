package views

// assetVersions maps a static filename to a short content hash, set by the
// server at startup. Used to fingerprint asset URLs so the browser always
// fetches the current file after an edit/deploy (cache-busting).
var assetVersions = map[string]string{}

// SetAssetVersions is called once by the server with content hashes.
func SetAssetVersions(m map[string]string) { assetVersions = m }

// Asset returns the fingerprinted URL for a static file.
func Asset(name string) string {
	if v := assetVersions[name]; v != "" {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}
