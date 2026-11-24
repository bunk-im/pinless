package main

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed templates/*
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

type SearchResult struct {
	Images   []string `json:"images"`
	Bookmark string   `json:"bookmark,omitempty"`
}

type Pin struct {
	ID          string `json:"id"`
	Link        string `json:"link"`
	ImageWidth  int    `json:"width"`
	ImageHeight int    `json:"height"`
	ImageURL    string `json:"image_url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	PinnerName  string `json:"pinner_name"`
	PinnerUser  string `json:"pinner_user"`
}

type PinData struct {
	Pin     Pin
	Related []Pin
}

type Profile struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	FullName      string `json:"full_name"`
	FirstName     string `json:"first_name"`
	About         string `json:"about"`
	PinCount      int    `json:"pin_count"`
	BoardCount    int    `json:"board_count"`
	FollowerCount int    `json:"follower_count"`
	ImageURL      string `json:"image_url"`
	ImageLargeURL string `json:"image_large_url"`
	WebsiteURL    string `json:"website_url"`
}

type Board struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	PinCount  int    `json:"pin_count"`
	ImageURL  string `json:"image_url"`
	OwnerName string `json:"owner_name"`
	OwnerUser string `json:"owner_user"`
}

var allowedDomains = []string{"pinimg.com", "i.pinimg.com", "pinterest.com"}

var (
	pinterestCookieHeader string
	pinterestCSRFToken    string
)

func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" || value == "" || os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, value)
	}
}

func useLoginEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("USE_LOGIN")))
	switch value {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

func initPinterestAuth() {
	if !useLoginEnabled() {
		fmt.Println("Pinterest auth: disabled by USE_LOGIN")
		return
	}

	session := strings.TrimSpace(os.Getenv("PINTEREST_SESSION"))
	csrf := strings.TrimSpace(os.Getenv("PINTEREST_CSRF_TOKEN"))
	if session != "" || csrf != "" {
		if session == "" || csrf == "" {
			fmt.Fprintln(os.Stderr, "Pinterest auth: PINTEREST_SESSION and PINTEREST_CSRF_TOKEN must both be set")
			return
		}
		pinterestCSRFToken = csrf
		pinterestCookieHeader = fmt.Sprintf("csrftoken=%s; _pinterest_sess=%s", csrf, session)
		fmt.Println("Pinterest auth: enabled")
		return
	}

	raw := strings.TrimSpace(os.Getenv("PINTEREST_COOKIES"))
	if raw == "" {
		path := strings.TrimSpace(os.Getenv("PINTEREST_COOKIES_FILE"))
		if path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Pinterest auth: failed to read PINTEREST_COOKIES_FILE")
			return
		}
		raw = strings.TrimSpace(string(data))
	}
	if raw == "" {
		return
	}

	raw = strings.TrimSpace(strings.TrimPrefix(raw, "Cookie:"))
	values := map[string]string{}
	if strings.HasPrefix(raw, "[") {
		var cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &cookies); err != nil {
			fmt.Fprintln(os.Stderr, "Pinterest auth: cookie JSON is invalid")
			return
		}
		for _, cookie := range cookies {
			if cookie.Name == "csrftoken" || cookie.Name == "_pinterest_sess" {
				values[cookie.Name] = cookie.Value
			}
		}
	} else {
		for _, part := range strings.Split(raw, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && (key == "csrftoken" || key == "_pinterest_sess") {
				values[key] = value
			}
		}
	}

	session = values["_pinterest_sess"]
	csrf = values["csrftoken"]
	if session == "" || csrf == "" {
		fmt.Fprintln(os.Stderr, "Pinterest auth: both csrftoken and _pinterest_sess cookies are required")
		return
	}
	pinterestCSRFToken = csrf
	pinterestCookieHeader = fmt.Sprintf("csrftoken=%s; _pinterest_sess=%s", csrf, session)
	fmt.Println("Pinterest auth: enabled")
}

func setPinterestAuth(req *http.Request, csrftoken string) {
	token := pinterestCSRFToken
	if token == "" {
		token = csrftoken
	}
	if token != "" {
		req.Header.Set("x-csrftoken", token)
	}

	if pinterestCookieHeader != "" {
		req.Header.Set("cookie", pinterestCookieHeader)
	} else if token != "" {
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", token))
	}
}

func isNumericPinID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func pinLink(pinID string, imageURL string) string {
	if pinterestCookieHeader != "" && isNumericPinID(pinID) {
		return "/pin/" + pinID
	}
	return imageURL
}

func absURL(c *gin.Context, path string) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, path)
}

type PinterestImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type PinterestImages struct {
	Orig     PinterestImage `json:"orig"`
	Size736x PinterestImage `json:"736x"`
	Size474x PinterestImage `json:"474x"`
	Size564x PinterestImage `json:"564x"`
	Size236x PinterestImage `json:"236x"`
}

func extractBestImageInfo(pi PinterestImages) *PinterestImage {
	if pi.Orig.URL != "" {
		return &pi.Orig
	} else if pi.Size736x.URL != "" {
		return &pi.Size736x
	} else if pi.Size564x.URL != "" {
		return &pi.Size564x
	} else if pi.Size474x.URL != "" {
		return &pi.Size474x
	} else if pi.Size236x.URL != "" {
		return &pi.Size236x
	}
	return nil
}

func main() {
	loadDotEnv()
	initPinterestAuth()

	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	router := gin.Default()

	router.Use(func(c *gin.Context) {
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; form-action 'self'; base-uri 'self'; frame-ancestors 'none'")
		c.SetSameSite(http.SameSiteStrictMode)
		c.Next()
	})

	// Serve embedded static files
	staticSubFS, _ := fs.Sub(staticFS, "static")
	router.StaticFS("/static", http.FS(staticSubFS))

	// Load embedded templates
	tmpl := template.Must(template.ParseFS(templatesFS, "templates/*"))
	router.SetHTMLTemplate(tmpl)

	router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	router.GET("/search/pins/", searchHandler)
	router.GET("/pin/:id", pinHandler)
	router.GET("/image", proxyImageHandler)
	router.GET("/about", func(c *gin.Context) {
		c.HTML(http.StatusOK, "about.html", nil)
	})
	router.GET("/:username/", profileRedirectHandler)
	router.GET("/:username/_saved/", savedHandler)
	router.GET("/:username/_created/", createdHandler)
	router.GET("/:username/:slug", boardHandler)

	fmt.Println(` _____ _     _             
|  _  |_|___| |___ ___ ___ 
|   __| |   | | -_|_ -|_ -|
|__|  |_|_|_|_|___|___|___|
`)
	fmt.Println("Server running at http://0.0.0.0:3000")

	router.Run(":3000")
}

func searchHandler(c *gin.Context) {
	query := c.Query("q")

	// get bookmark from cookie for privacy
	bookmark := ""
	if cookie, err := c.Cookie("bookmark"); err == nil && cookie != "" {
		bookmark = cookie
	}

	// clear bookmark if new search
	if _, nextExists := c.GetQuery("next"); !nextExists {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
		bookmark = ""
	}

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
	}
	if csrftoken == "" {
		csrftoken = fetchCSRFToken()
		if csrftoken != "" {
			if pinterestCookieHeader == "" {
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
			}
		}
	}

	apiURL := "https://www.pinterest.com/resource/BaseSearchResource/get/"
	options := map[string]interface{}{
		"query": query,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParamObj := map[string]interface{}{"options": options}

	dataParam, err := json.Marshal(dataParamObj)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encode data"})
		return
	}

	dataParamEscaped := url.QueryEscape(string(dataParam))
	finalURL := fmt.Sprintf("%s?data=%s", apiURL, dataParamEscaped)

	method := http.MethodGet
	var body io.Reader
	if bookmark != "" {
		method = http.MethodPost
		finalURL = apiURL
		body = strings.NewReader("data=" + dataParamEscaped)
	}

	req, err := http.NewRequest(method, finalURL, body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	req.Header.Set("x-pinterest-pws-handler", "www/search/[scope].js")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Request failed"})
		return
	}
	defer resp.Body.Close()

	if newToken := resp.Cookies(); len(newToken) > 0 {
		for _, ck := range newToken {
			if ck != nil && ck.Name == "csrftoken" && ck.Value != "" {
				csrftoken = ck.Value
				if pinterestCookieHeader == "" {
					c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
				}
				break
			}
		}
	}

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, gzErr := gzip.NewReader(resp.Body)
		if gzErr != nil {
			c.HTML(http.StatusBadGateway, "results.html", gin.H{
				"Results": nil,
				"Query":   query,
				"Error": gin.H{
					"error":            "Failed to init gzip reader",
					"upstream_status":  resp.Status,
					"content_encoding": resp.Header.Get("Content-Encoding"),
					"content_type":     resp.Header.Get("Content-Type"),
					"details":          gzErr.Error(),
				},
			})
			return
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read response"})
		return
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(bodyBytes)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		c.HTML(http.StatusBadGateway, "results.html", gin.H{
			"Results": nil,
			"Query":   query,
			"Error": gin.H{
				"error":            "Upstream error",
				"upstream_status":  resp.Status,
				"content_encoding": resp.Header.Get("Content-Encoding"),
				"content_type":     resp.Header.Get("Content-Type"),
				"body":             snippet,
			},
		})
		return
	}

	var responseData struct {
		ResourceResponse struct {
			Data struct {
				Results []struct {
					ID     string          `json:"id"`
					Images PinterestImages `json:"images"`
				} `json:"results"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		snippet := string(bodyBytes)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		c.HTML(http.StatusBadGateway, "results.html", gin.H{
			"Results": nil,
			"Query":   query,
			"Error": gin.H{
				"error":            "Failed to decode response",
				"upstream_status":  resp.Status,
				"content_encoding": resp.Header.Get("Content-Encoding"),
				"content_type":     resp.Header.Get("Content-Type"),
				"decode_error":     err.Error(),
				"body":             snippet,
			},
		})
		return
	}

	// store bookmark for pagination
	if responseData.ResourceResponse.Bookmark != "" {
		c.SetCookie("bookmark", responseData.ResourceResponse.Bookmark, 0, "/", "", c.Request.TLS != nil, true)
	} else {
		// clear cookie when no pages
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	type ResultItem struct {
		Link        string
		Image       string
		ImageWidth  int
		ImageHeight int
	}

	var results []ResultItem
	for _, result := range responseData.ResourceResponse.Data.Results {
		imageUrl := result.Images.Orig.URL
		if imageUrl != "" && isAllowedDomain(imageUrl) {
			proxyImageUrl := fmt.Sprintf("/image?url=%s", url.QueryEscape(imageUrl))
			link := proxyImageUrl
			if pinterestCookieHeader != "" && isNumericPinID(result.ID) {
				link = "/pin/" + result.ID
				if query != "" {
					link += "?q=" + url.QueryEscape(query)
				}
			}
			results = append(results, ResultItem{
				Link:        link,
				Image:       proxyImageUrl,
				ImageWidth:  result.Images.Orig.Width,
				ImageHeight: result.Images.Orig.Height,
			})
		}
	}

	c.HTML(http.StatusOK, "results.html", gin.H{
		"Results":  results,
		"Bookmark": responseData.ResourceResponse.Bookmark,
		"Query":    query,
	})
}

func pinHandler(c *gin.Context) {
	pinID := c.Param("id")
	query := c.Query("q")
	from := c.Query("from")

	bookmark := ""
	if cookie, err := c.Cookie("bookmark"); err == nil && cookie != "" {
		bookmark = cookie
	}

	if _, nextExists := c.GetQuery("next"); !nextExists {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
		bookmark = ""
	}

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
	}
	if csrftoken == "" {
		csrftoken = fetchCSRFToken()
		if csrftoken != "" {
			if pinterestCookieHeader == "" {
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
			}
		}
	}

	pin, err := fetchPinDetails(pinID, csrftoken)
	if err != nil {
		status := http.StatusBadGateway
		message := "Couldn't load this pin. Please try again later."
		var fetchErr *pinFetchError
		if errors.As(err, &fetchErr) && fetchErr.httpStatus == http.StatusNotFound {
			status = http.StatusNotFound
			message = "Pin not found."
		}
		c.HTML(status, "pin.html", gin.H{
			"Pin":     Pin{ID: pinID},
			"Error":   message,
			"Query":   query,
			"From":    from,
			"PageURL": absURL(c, c.Request.URL.RequestURI()),
		})
		return
	}

	related, nextBookmark := fetchRelatedPins(pinID, csrftoken, bookmark)

	if nextBookmark != "" {
		c.SetCookie("bookmark", nextBookmark, 0, "/", "", c.Request.TLS != nil, true)
	} else {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	ogImage := ""
	if pin.ImageURL != "" {
		ogImage = absURL(c, pin.ImageURL)
	}

	c.HTML(http.StatusOK, "pin.html", gin.H{
		"Pin":             pin,
		"Related":         related,
		"RelatedBookmark": nextBookmark,
		"Query":           query,
		"From":            from,
		"PageURL":         absURL(c, c.Request.URL.RequestURI()),
		"OGImage":         ogImage,
	})
}

type pinFetchError struct {
	httpStatus int
	message    string
}

func (e *pinFetchError) Error() string {
	return e.message
}

func fetchPinDetails(pinID string, csrftoken string) (Pin, error) {
	apiURL := "https://www.pinterest.com/resource/PinResource/get/"
	sourceURL := fmt.Sprintf("/pin/%s/", pinID)
	options := map[string]interface{}{
		"id": pinID,
	}
	dataParamObj := map[string]interface{}{"options": options}
	dataParam, err := json.Marshal(dataParamObj)
	if err != nil {
		return Pin{}, fmt.Errorf("failed to encode pin request: %w", err)
	}
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)
	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s", apiURL, sourceURLEscaped, dataParamEscaped)

	req, err := http.NewRequest(http.MethodGet, finalURL, nil)
	if err != nil {
		return Pin{}, fmt.Errorf("failed to create pin request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/pin/%s.js", pinID))
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Pin{}, &pinFetchError{httpStatus: http.StatusBadGateway, message: "Could not reach Pinterest."}
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return Pin{}, &pinFetchError{httpStatus: http.StatusBadGateway, message: "Pinterest returned an unreadable response."}
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		return Pin{}, &pinFetchError{httpStatus: http.StatusBadGateway, message: "Pinterest returned an unreadable response."}
	}

	var singlePinResponse struct {
		ResourceResponse struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Error   struct {
				HTTPStatus int    `json:"http_status"`
				Message    string `json:"message"`
			} `json:"error"`
			Data struct {
				Title       string          `json:"title"`
				Description string          `json:"description"`
				Images      PinterestImages `json:"images"`
				Pinner      struct {
					FullName string `json:"full_name"`
					Username string `json:"username"`
				} `json:"pinner"`
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"data"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &singlePinResponse); err != nil {
		return Pin{}, &pinFetchError{
			httpStatus: resp.StatusCode,
			message:    fmt.Sprintf("Pinterest returned %s.", resp.Status),
		}
	}

	rr := singlePinResponse.ResourceResponse
	if resp.StatusCode != http.StatusOK || rr.Status == "error" {
		message := strings.TrimSpace(rr.Error.Message)
		if message == "" {
			message = strings.TrimSpace(rr.Message)
		}
		if message == "" {
			message = fmt.Sprintf("Pinterest returned %s.", resp.Status)
		}
		status := resp.StatusCode
		if rr.Error.HTTPStatus != 0 {
			status = rr.Error.HTTPStatus
		}
		if status == 0 {
			status = http.StatusBadGateway
		}
		return Pin{}, &pinFetchError{httpStatus: status, message: message}
	}

	data := rr.Data
	if data.ID == "" {
		return Pin{}, &pinFetchError{httpStatus: http.StatusNotFound, message: "Pin not found."}
	}

	pin := Pin{
		ID:          pinID,
		Title:       strings.TrimSpace(data.Title),
		Description: strings.TrimSpace(data.Description),
		PinnerName:  strings.TrimSpace(data.Pinner.FullName),
		PinnerUser:  strings.TrimSpace(data.Pinner.Username),
	}

	bestImage := extractBestImageInfo(data.Images)
	if bestImage != nil {
		pin.ImageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(bestImage.URL))
		pin.ImageHeight = bestImage.Height
		pin.ImageWidth = bestImage.Width
	}

	return pin, nil
}

func fetchRelatedPins(pinID string, csrftoken string, bookmark string) ([]Pin, string) {
	apiURL := "https://www.pinterest.com/resource/RelatedModulesResource/get/"
	sourceURL := fmt.Sprintf("/pin/%s/", pinID)
	options := map[string]interface{}{
		"pin_id":    pinID,
		"page_size": 12,
		"source":    "pin",
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParamObj := map[string]interface{}{"options": options}
	dataParam, _ := json.Marshal(dataParamObj)
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)

	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s", apiURL, sourceURLEscaped, dataParamEscaped)

	method := http.MethodGet
	var body io.Reader
	if bookmark != "" {
		method = http.MethodPost
		finalURL = apiURL
		body = strings.NewReader("data=" + dataParamEscaped)
	}

	req, _ := http.NewRequest(method, finalURL, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/pin/%s.js", pinID))
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return []Pin{}, ""
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return []Pin{}, ""
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var responseData struct {
		ResourceResponse struct {
			Data []struct {
				ID          string          `json:"id"`
				Type        string          `json:"type"`
				StoryType   string          `json:"story_type"`
				TitleRaw    json.RawMessage `json:"title"`
				GridTitle   string          `json:"grid_title"`
				Description string          `json:"description"`
				Images      PinterestImages `json:"images"`
				Pinner      struct {
					FullName string `json:"full_name"`
					Username string `json:"username"`
				} `json:"pinner"`
				AggregatedPinData struct {
					AggregatedStats struct {
						Saves int `json:"saves"`
					} `json:"aggregated_stats"`
				} `json:"aggregated_pin_data"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return []Pin{}, ""
	}

	var related []Pin
	pinCount := 0
	for _, result := range responseData.ResourceResponse.Data {
		if result.Type != "pin" || result.ID == "" {
			continue
		}
		pinCount++

		title := ""
		if len(result.TitleRaw) > 0 {
			var titleObj struct {
				Format string   `json:"format"`
				Args   []string `json:"args"`
			}
			if err := json.Unmarshal(result.TitleRaw, &titleObj); err == nil && titleObj.Format != "" {
				title = titleObj.Format
			} else {
				var titleStr string
				if err := json.Unmarshal(result.TitleRaw, &titleStr); err == nil {
					title = titleStr
				}
			}
		}
		if title == "" {
			title = result.GridTitle
		}
		if title == "" {
			title = result.Description
		}

		bestImage := extractBestImageInfo(result.Images)
		if bestImage != nil {
			imageURL := fmt.Sprintf("/image?url=%s", url.QueryEscape(bestImage.URL))
			related = append(related, Pin{
				ID:          result.ID,
				Link:        pinLink(result.ID, imageURL),
				ImageURL:    imageURL,
				ImageWidth:  bestImage.Width,
				ImageHeight: bestImage.Height,
				Title:       strings.TrimSpace(title),
				PinnerName:  strings.TrimSpace(result.Pinner.FullName),
			})
		}
	}

	return related, responseData.ResourceResponse.Bookmark
}

func profileRedirectHandler(c *gin.Context) {
	username := c.Param("username")
	c.Redirect(http.StatusFound, "/"+username+"/_saved/")
}

func savedHandler(c *gin.Context) {
	username := c.Param("username")

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
	}
	if csrftoken == "" {
		csrftoken = fetchCSRFToken()
		if csrftoken != "" {
			if pinterestCookieHeader == "" {
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
			}
		}
	}

	bookmark := ""
	if cookie, err := c.Cookie("bookmark"); err == nil && cookie != "" {
		bookmark = cookie
	}
	if _, nextExists := c.GetQuery("next"); !nextExists {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
		bookmark = ""
	}

	pinsBookmark := ""
	if cookie, err := c.Cookie("pins_bookmark"); err == nil && cookie != "" {
		pinsBookmark = cookie
	}
	if _, nextExists := c.GetQuery("pins_next"); !nextExists {
		c.SetCookie("pins_bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
		pinsBookmark = ""
	}

	profile, profileToken := fetchUserProfile(username, csrftoken)
	if profileToken != "" {
		csrftoken = profileToken
		if pinterestCookieHeader == "" {
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
		}
	}

	boards, nextBookmark, boardToken := fetchBoards(username, csrftoken, bookmark)
	if boardToken != "" {
		csrftoken = boardToken
		if pinterestCookieHeader == "" {
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
		}
	}

	if nextBookmark != "" {
		c.SetCookie("bookmark", nextBookmark, 0, "/", "", c.Request.TLS != nil, true)
	} else {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	profilePins, pinsNextBookmark := fetchUserProfilePins(username, csrftoken, pinsBookmark)

	if pinsNextBookmark != "" {
		c.SetCookie("pins_bookmark", pinsNextBookmark, 0, "/", "", c.Request.TLS != nil, true)
	} else {
		c.SetCookie("pins_bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	ogImage := ""
	if profile.ImageURL != "" {
		ogImage = absURL(c, profile.ImageURL)
	}

	c.HTML(http.StatusOK, "profile.html", gin.H{
		"Profile":      profile,
		"Boards":       boards,
		"ProfilePins":  profilePins,
		"Bookmark":     nextBookmark,
		"PinsBookmark": pinsNextBookmark,
		"PageURL":      absURL(c, c.Request.URL.RequestURI()),
		"OGImage":      ogImage,
	})
}

func createdHandler(c *gin.Context) {
	username := c.Param("username")

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
	}
	if csrftoken == "" {
		csrftoken = fetchCSRFToken()
		if csrftoken != "" {
			if pinterestCookieHeader == "" {
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
			}
		}
	}

	bookmark := ""
	if cookie, err := c.Cookie("bookmark"); err == nil && cookie != "" {
		bookmark = cookie
	}
	if _, nextExists := c.GetQuery("next"); !nextExists {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
		bookmark = ""
	}

	profile, profileToken := fetchUserProfile(username, csrftoken)
	if profileToken != "" {
		csrftoken = profileToken
		if pinterestCookieHeader == "" {
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
		}
	}

	pins, nextBookmark := fetchUserCreatedPins(profile.ID, username, csrftoken, bookmark)

	if nextBookmark != "" {
		c.SetCookie("bookmark", nextBookmark, 0, "/", "", c.Request.TLS != nil, true)
	} else {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	ogImage := ""
	if profile.ImageURL != "" {
		ogImage = absURL(c, profile.ImageURL)
	}

	c.HTML(http.StatusOK, "profile.html", gin.H{
		"Profile":  profile,
		"Pins":     pins,
		"Bookmark": nextBookmark,
		"PageURL":  absURL(c, c.Request.URL.RequestURI()),
		"OGImage":  ogImage,
	})
}

func boardHandler(c *gin.Context) {
	username := c.Param("username")
	slug := c.Param("slug")

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
	}
	if csrftoken == "" {
		csrftoken = fetchCSRFToken()
		if csrftoken != "" {
			if pinterestCookieHeader == "" {
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
			}
		}
	}

	bookmark := ""
	if cookie, err := c.Cookie("bookmark"); err == nil && cookie != "" {
		bookmark = cookie
	}
	if _, nextExists := c.GetQuery("next"); !nextExists {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
		bookmark = ""
	}

	boardInfo := fetchBoardInfo(username, slug, csrftoken)
	var pins []Pin
	var nextBookmark string
	var feedToken string
	if boardInfo.ID != "" {
		sourceURL := fmt.Sprintf("/%s/%s/", username, slug)
		pins, nextBookmark, feedToken = fetchBoardPins(boardInfo.ID, sourceURL, csrftoken, bookmark)
		if feedToken != "" {
			csrftoken = feedToken
			if pinterestCookieHeader == "" {
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
			}
		}
	}

	if nextBookmark != "" {
		c.SetCookie("bookmark", nextBookmark, 0, "/", "", c.Request.TLS != nil, true)
	} else {
		c.SetCookie("bookmark", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	ogImage := ""
	if boardInfo.ImageURL != "" {
		ogImage = absURL(c, boardInfo.ImageURL)
	}

	c.HTML(http.StatusOK, "board.html", gin.H{
		"Board":    boardInfo,
		"Pins":     pins,
		"Bookmark": nextBookmark,
		"PageURL":  absURL(c, c.Request.URL.RequestURI()),
		"OGImage":  ogImage,
	})
}

func fetchUserProfile(username string, csrftoken string) (Profile, string) {
	apiURL := "https://www.pinterest.com/resource/UserResource/get/"
	sourceURL := fmt.Sprintf("/%s/", username)
	options := map[string]interface{}{
		"username":      username,
		"field_set_key": "profile",
	}
	dataParamObj := map[string]interface{}{"options": options}
	dataParam, _ := json.Marshal(dataParamObj)
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)
	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s", apiURL, sourceURLEscaped, dataParamEscaped)

	req, _ := http.NewRequest(http.MethodGet, finalURL, nil)
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/%s.js", username))
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Profile{}, ""
	}
	defer resp.Body.Close()

	newToken := ""
	for _, ck := range resp.Cookies() {
		if ck != nil && ck.Name == "csrftoken" && ck.Value != "" {
			newToken = ck.Value
			break
		}
	}

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return Profile{}, newToken
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var responseData struct {
		ResourceResponse struct {
			Data struct {
				ID             string `json:"id"`
				Username       string `json:"username"`
				FullName       string `json:"full_name"`
				FirstName      string `json:"first_name"`
				About          string `json:"about"`
				PinCount       int    `json:"pin_count"`
				BoardCount     int    `json:"board_count"`
				FollowerCount  int    `json:"follower_count"`
				ImageLargeURL  string `json:"image_large_url"`
				ImageMediumURL string `json:"image_medium_url"`
				WebsiteURL     string `json:"website_url"`
			} `json:"data"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return Profile{}, newToken
	}

	d := responseData.ResourceResponse.Data
	imageURL := ""
	if d.ImageLargeURL != "" {
		imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(d.ImageLargeURL))
	} else if d.ImageMediumURL != "" {
		imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(d.ImageMediumURL))
	}

	return Profile{
		ID:            d.ID,
		Username:      d.Username,
		FullName:      d.FullName,
		FirstName:     d.FirstName,
		About:         d.About,
		PinCount:      d.PinCount,
		BoardCount:    d.BoardCount,
		FollowerCount: d.FollowerCount,
		ImageURL:      imageURL,
		WebsiteURL:    d.WebsiteURL,
	}, newToken
}

func fetchBoards(username string, csrftoken string, bookmark string) ([]Board, string, string) {
	apiURL := "https://www.pinterest.com/resource/BoardsFeedResource/get/"
	sourceURL := fmt.Sprintf("/%s/", username)
	options := map[string]interface{}{
		"field_set_key":  "profile_grid_item",
		"filter_stories": false,
		"sort":           "last_pinned_to",
		"username":       username,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParamObj := map[string]interface{}{"options": options}
	dataParam, _ := json.Marshal(dataParamObj)
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)

	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s", apiURL, sourceURLEscaped, dataParamEscaped)

	method := http.MethodGet
	var body io.Reader
	if bookmark != "" {
		method = http.MethodPost
		finalURL = apiURL
		body = strings.NewReader("data=" + dataParamEscaped)
	}

	req, _ := http.NewRequest(method, finalURL, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/%s.js", username))
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", ""
	}
	defer resp.Body.Close()

	newToken := ""
	for _, ck := range resp.Cookies() {
		if ck != nil && ck.Name == "csrftoken" && ck.Value != "" {
			newToken = ck.Value
			break
		}
	}

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, "", newToken
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var responseData struct {
		ResourceResponse struct {
			Data []struct {
				ID          string `json:"id"`
				Type        string `json:"type"`
				Name        string `json:"name"`
				URL         string `json:"url"`
				PinCount    int    `json:"pin_count"`
				CoverImages struct {
					Cover200x150 *struct {
						URL string `json:"url"`
					} `json:"200x150"`
				} `json:"cover_images"`
				ImageCoverURL string `json:"image_cover_url"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return nil, "", newToken
	}

	var boards []Board
	for _, r := range responseData.ResourceResponse.Data {
		if r.Type != "board" || r.ID == "" {
			continue
		}
		imageURL := ""
		if r.CoverImages.Cover200x150 != nil {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(r.CoverImages.Cover200x150.URL))
		} else if r.ImageCoverURL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(r.ImageCoverURL))
		}
		boards = append(boards, Board{
			ID:       r.ID,
			Name:     r.Name,
			URL:      r.URL,
			PinCount: r.PinCount,
			ImageURL: imageURL,
		})
	}

	return boards, responseData.ResourceResponse.Bookmark, newToken
}

func fetchUserCreatedPins(userID string, username string, csrftoken string, bookmark string) ([]Pin, string) {
	apiURL := "https://www.pinterest.com/resource/UserActivityPinsResource/get/"
	sourceURL := fmt.Sprintf("/%s/_created/", username)
	options := map[string]interface{}{
		"exclude_add_pin_rep": true,
		"field_set_key":       "profile_created_grid_item",
		"is_own_profile_pins": false,
		"user_id":             userID,
		"username":            username,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParamObj := map[string]interface{}{"options": options}
	dataParam, _ := json.Marshal(dataParamObj)
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)

	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s&_=%d", apiURL, sourceURLEscaped, dataParamEscaped, time.Now().UnixMilli())

	method := http.MethodGet
	var body io.Reader
	if bookmark != "" {
		method = http.MethodPost
		finalURL = apiURL
		body = strings.NewReader("data=" + dataParamEscaped)
	}

	req, _ := http.NewRequest(method, finalURL, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/%s/_created.js", username))
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, ""
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var responseData struct {
		ResourceResponse struct {
			Data []struct {
				ID     string          `json:"id"`
				Type   string          `json:"type"`
				Images PinterestImages `json:"images"`
				Pinner struct {
					FullName string `json:"full_name"`
					Username string `json:"username"`
				} `json:"pinner"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return nil, ""
	}

	var pins []Pin
	for _, result := range responseData.ResourceResponse.Data {
		if result.Type != "pin" || result.ID == "" {
			continue
		}

		bestImage := extractBestImageInfo(result.Images)
		if bestImage != nil {
			imageURL := fmt.Sprintf("/image?url=%s", url.QueryEscape(bestImage.URL))
			pins = append(pins, Pin{
				ID:          result.ID,
				Link:        pinLink(result.ID, imageURL),
				ImageURL:    imageURL,
				ImageWidth:  bestImage.Width,
				ImageHeight: bestImage.Height,
				PinnerName:  strings.TrimSpace(result.Pinner.FullName),
			})
		}
	}

	return pins, responseData.ResourceResponse.Bookmark
}

func fetchUserProfilePins(username string, csrftoken string, bookmark string) ([]Pin, string) {
	apiURL := "https://www.pinterest.com/resource/UserPinsResource/get/"
	sourceURL := fmt.Sprintf("/%s/", username)
	options := map[string]interface{}{
		"add_vase":            true,
		"field_set_key":       "mobile_grid_item",
		"is_own_profile_pins": false,
		"username":            username,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParamObj := map[string]interface{}{
		"options": options,
		"context": map[string]interface{}{},
	}
	dataParam, _ := json.Marshal(dataParamObj)
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)

	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s&_=%d", apiURL, sourceURLEscaped, dataParamEscaped, time.Now().UnixMilli())

	method := http.MethodGet
	var body io.Reader
	if bookmark != "" {
		method = http.MethodPost
		finalURL = apiURL
		body = strings.NewReader("data=" + dataParamEscaped)
	}

	req, _ := http.NewRequest(method, finalURL, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/%s.js", username))
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, ""
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var responseData struct {
		ResourceResponse struct {
			Data []struct {
				ID     string          `json:"id"`
				Type   string          `json:"type"`
				Images PinterestImages `json:"images"`
				Pinner struct {
					FullName string `json:"full_name"`
					Username string `json:"username"`
				} `json:"pinner"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return nil, ""
	}

	var pins []Pin
	for _, result := range responseData.ResourceResponse.Data {
		if result.Type != "pin" || result.ID == "" {
			continue
		}

		bestImage := extractBestImageInfo(result.Images)
		if bestImage != nil {
			imageURL := fmt.Sprintf("/image?url=%s", url.QueryEscape(bestImage.URL))
			pins = append(pins, Pin{
				ID:          result.ID,
				Link:        pinLink(result.ID, imageURL),
				ImageURL:    imageURL,
				ImageWidth:  bestImage.Width,
				ImageHeight: bestImage.Height,
				PinnerName:  strings.TrimSpace(result.Pinner.FullName),
			})
		}
	}

	return pins, responseData.ResourceResponse.Bookmark
}

func fetchBoardInfo(username string, slug string, csrftoken string) Board {
	apiURL := "https://www.pinterest.com/resource/BoardResource/get/"
	sourceURL := fmt.Sprintf("/%s/%s/", username, slug)
	options := map[string]interface{}{
		"field_set_key":  "profile_grid_item",
		"is_mobile_fork": true,
		"username":       username,
		"slug":           slug,
	}
	dataParamObj := map[string]interface{}{"options": options}
	dataParam, _ := json.Marshal(dataParamObj)
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)
	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s", apiURL, sourceURLEscaped, dataParamEscaped)

	req, _ := http.NewRequest(http.MethodGet, finalURL, nil)
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/%s/%s.js", username, slug))
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Board{}
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return Board{}
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var responseData struct {
		ResourceResponse struct {
			Data struct {
				ID            string `json:"id"`
				Name          string `json:"name"`
				URL           string `json:"url"`
				PinCount      int    `json:"pin_count"`
				ImageCoverURL string `json:"image_cover_url"`
				Description   string `json:"description"`
				Owner         struct {
					Username string `json:"username"`
					FullName string `json:"full_name"`
				} `json:"owner"`
			} `json:"data"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return Board{}
	}

	d := responseData.ResourceResponse.Data
	imageURL := ""
	if d.ImageCoverURL != "" {
		imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(d.ImageCoverURL))
	}

	return Board{
		ID:        d.ID,
		Name:      d.Name,
		URL:       d.URL,
		PinCount:  d.PinCount,
		ImageURL:  imageURL,
		OwnerName: d.Owner.FullName,
		OwnerUser: d.Owner.Username,
	}
}

func fetchBoardPins(boardID string, sourceURL string, csrftoken string, bookmark string) ([]Pin, string, string) {
	apiURL := "https://www.pinterest.com/resource/BoardFeedResource/get/"
	options := map[string]interface{}{
		"add_vase":             true,
		"board_id":             boardID,
		"field_set_key":        "react_grid_pin",
		"filter_section_pins":  false,
		"is_react":             true,
		"prepend":              false,
		"page_size":            15,
		"redux_normalize_feed": true,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	} else {
		options["bookmarks"] = nil
	}
	dataParamObj := map[string]interface{}{
		"options": options,
		"context": map[string]interface{}{},
	}
	dataParam, _ := json.Marshal(dataParamObj)
	dataParamEscaped := url.QueryEscape(string(dataParam))
	sourceURLEscaped := url.QueryEscape(sourceURL)
	finalURL := fmt.Sprintf("%s?source_url=%s&data=%s&_=%d", apiURL, sourceURLEscaped, dataParamEscaped, time.Now().UnixMilli())

	method := http.MethodGet
	var body io.Reader
	if bookmark != "" {
		method = http.MethodPost
		finalURL = apiURL
		body = strings.NewReader("data=" + dataParamEscaped)
	}

	req, _ := http.NewRequest(method, finalURL, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("x-pinterest-pws-handler", "www/[username]/[slug].js")
	req.Header.Set("Referer", "https://www.pinterest.com/")

	setPinterestAuth(req, csrftoken)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", ""
	}
	defer resp.Body.Close()

	newToken := ""
	for _, ck := range resp.Cookies() {
		if ck != nil && ck.Name == "csrftoken" && ck.Value != "" {
			newToken = ck.Value
			break
		}
	}

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, "", newToken
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var responseData struct {
		ResourceResponse struct {
			Data []struct {
				ID     string          `json:"id"`
				Type   string          `json:"type"`
				Images PinterestImages `json:"images"`
				Pinner struct {
					FullName string `json:"full_name"`
					Username string `json:"username"`
				} `json:"pinner"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return nil, "", newToken
	}

	var pins []Pin
	for _, result := range responseData.ResourceResponse.Data {
		if result.Type != "pin" || result.ID == "" {
			continue
		}

		bestImage := extractBestImageInfo(result.Images)
		if bestImage != nil {
			imageURL := fmt.Sprintf("/image?url=%s", url.QueryEscape(bestImage.URL))
			pins = append(pins, Pin{
				ID:          result.ID,
				Link:        pinLink(result.ID, imageURL),
				ImageURL:    imageURL,
				ImageWidth:  bestImage.Width,
				ImageHeight: bestImage.Height,
				PinnerName:  strings.TrimSpace(result.Pinner.FullName),
			})
		}
	}

	return pins, responseData.ResourceResponse.Bookmark, newToken
}

func fetchCSRFToken() string {
	if pinterestCSRFToken != "" {
		return ""
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, "https://www.pinterest.com/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	for _, ck := range resp.Cookies() {
		if ck.Name == "csrftoken" && ck.Value != "" {
			return ck.Value
		}
	}
	return ""
}

func proxyImageHandler(c *gin.Context) {
	imageUrl := c.Query("url")
	if !isAllowedDomain(imageUrl) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Domain not allowed"})
		return
	}

	imageSrc, contentType, err := fetchImage(imageUrl)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch image"})
		return
	}

	c.Data(http.StatusOK, contentType, imageSrc)
}

func isAllowedDomain(urlStr string) bool {
	parsedUrl, err := url.Parse(urlStr)
	if err != nil || parsedUrl.Host == "" {
		return false
	}

	for _, domain := range allowedDomains {
		if parsedUrl.Host == domain || strings.HasSuffix(parsedUrl.Host, "."+domain) {
			return true
		}
	}

	return false
}

var safeClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !isAllowedDomain(req.URL.String()) {
			return fmt.Errorf("redirect to disallowed domain: %s", req.URL.Host)
		}
		return nil
	},
	Timeout: 10 * time.Second,
}

func fetchImage(imageUrl string) ([]byte, string, error) {
	resp, err := safeClient.Get(imageUrl)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("failed to fetch image: upstream returned %s", resp.Status)
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "image/") {
		contentType = "application/octet-stream"
	}

	imageBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image: %w", err)
	}

	return imageBytes, contentType, nil
}
