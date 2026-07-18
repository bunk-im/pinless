package main

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
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

func absURL(c *gin.Context, path string) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, path)
}

func main() {
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
	router.GET("/:username/", profileHandler)
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
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
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

	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", csrftoken))
	}

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
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
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
					ID     string `json:"id"`
					Images struct {
						Orig struct {
							URL string `json:"url"`
						} `json:"orig"`
					} `json:"images"`
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
		ID    string
		Image string
	}

	var results []ResultItem
	for _, result := range responseData.ResourceResponse.Data.Results {
		imageUrl := result.Images.Orig.URL
		if imageUrl != "" && isAllowedDomain(imageUrl) {
			proxyImageUrl := fmt.Sprintf("/image?url=%s", url.QueryEscape(imageUrl))
			results = append(results, ResultItem{
				ID:    result.ID,
				Image: proxyImageUrl,
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
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
		}
	}

	pin := fetchPinDetails(pinID, csrftoken)

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

func fetchPinDetails(pinID string, csrftoken string) Pin {
	apiURL := "https://www.pinterest.com/resource/PinResource/get/"
	sourceURL := fmt.Sprintf("/pin/%s/", pinID)
	options := map[string]interface{}{
		"id": pinID,
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
	req.Header.Set("x-pinterest-pws-handler", fmt.Sprintf("www/pin/%s.js", pinID))
	req.Header.Set("x-pinterest-source-url", sourceURL)
	req.Header.Set("Referer", "https://www.pinterest.com/")

	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", csrftoken))
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Pin{ID: pinID}
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	contentEncoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if strings.Contains(contentEncoding, "gzip") {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return Pin{ID: pinID}
		}
		defer gzr.Close()
		reader = gzr
	}

	bodyBytes, _ := io.ReadAll(reader)

	var singlePinResponse struct {
		ResourceResponse struct {
			Data struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Images      struct {
					Orig struct {
						URL string `json:"url"`
					} `json:"orig"`
					Size736x struct {
						URL string `json:"url"`
					} `json:"736x"`
					Size474x struct {
						URL string `json:"url"`
					} `json:"474x"`
					Size564x struct {
						URL string `json:"url"`
					} `json:"564x"`
					Size236x struct {
						URL string `json:"url"`
					} `json:"236x"`
				} `json:"images"`
				Pinner struct {
					FullName string `json:"full_name"`
					Username string `json:"username"`
				} `json:"pinner"`
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"data"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &singlePinResponse); err != nil {
		return Pin{ID: pinID}
	}

	data := singlePinResponse.ResourceResponse.Data

	pin := Pin{
		ID:          pinID,
		Title:       strings.TrimSpace(data.Title),
		Description: strings.TrimSpace(data.Description),
		PinnerName:  strings.TrimSpace(data.Pinner.FullName),
		PinnerUser:  strings.TrimSpace(data.Pinner.Username),
	}

	if data.Images.Orig.URL != "" {
		pin.ImageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(data.Images.Orig.URL))
	} else if data.Images.Size736x.URL != "" {
		pin.ImageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(data.Images.Size736x.URL))
	} else if data.Images.Size564x.URL != "" {
		pin.ImageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(data.Images.Size564x.URL))
	} else if data.Images.Size474x.URL != "" {
		pin.ImageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(data.Images.Size474x.URL))
	} else if data.Images.Size236x.URL != "" {
		pin.ImageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(data.Images.Size236x.URL))
	}

	return pin
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

	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", csrftoken))
	}

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
				Images      struct {
					Orig struct {
						URL string `json:"url"`
					} `json:"orig"`
					Size474x struct {
						URL string `json:"url"`
					} `json:"474x"`
					Size736x struct {
						URL string `json:"url"`
					} `json:"736x"`
					Size564x struct {
						URL string `json:"url"`
					} `json:"564x"`
					Size236x struct {
						URL string `json:"url"`
					} `json:"236x"`
				} `json:"images"`
				Pinner struct {
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

		var imageURL string
		if result.Images.Orig.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Orig.URL))
		} else if result.Images.Size736x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size736x.URL))
		} else if result.Images.Size564x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size564x.URL))
		} else if result.Images.Size474x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size474x.URL))
		} else if result.Images.Size236x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size236x.URL))
		}

		if imageURL != "" {
			related = append(related, Pin{
				ID:         result.ID,
				ImageURL:   imageURL,
				Title:      strings.TrimSpace(title),
				PinnerName: strings.TrimSpace(result.Pinner.FullName),
			})
		}
	}

	return related, responseData.ResourceResponse.Bookmark
}

func profileHandler(c *gin.Context) {
	username := c.Param("username")

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
	}
	if csrftoken == "" {
		csrftoken = fetchCSRFToken()
		if csrftoken != "" {
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
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
		c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
	}

	boards, nextBookmark, boardToken := fetchBoards(username, csrftoken, bookmark)
	if boardToken != "" {
		csrftoken = boardToken
		c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
	}

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
		"Profile":    profile,
		"Boards":     boards,
		"Bookmark":   nextBookmark,
		"PageURL":    absURL(c, c.Request.URL.RequestURI()),
		"OGImage":    ogImage,
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
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
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
			c.SetCookie("csrftoken", csrftoken, 0, "/", "", c.Request.TLS != nil, true)
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
		"username":     username,
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

	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", csrftoken))
	}

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
				ID            string `json:"id"`
				Username      string `json:"username"`
				FullName      string `json:"full_name"`
				FirstName     string `json:"first_name"`
				About         string `json:"about"`
				PinCount      int    `json:"pin_count"`
				BoardCount    int    `json:"board_count"`
				FollowerCount int    `json:"follower_count"`
				ImageLargeURL string `json:"image_large_url"`
				ImageMediumURL string `json:"image_medium_url"`
				WebsiteURL    string `json:"website_url"`
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
		"field_set_key": "profile_grid_item",
		"filter_stories": false,
		"sort":          "last_pinned_to",
		"username":      username,
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

	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", csrftoken))
	}

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
				ID       string `json:"id"`
				Type     string `json:"type"`
				Name     string `json:"name"`
				URL      string `json:"url"`
				PinCount int    `json:"pin_count"`
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

func fetchBoardInfo(username string, slug string, csrftoken string) Board {
	apiURL := "https://www.pinterest.com/resource/BoardResource/get/"
	sourceURL := fmt.Sprintf("/%s/%s/", username, slug)
	options := map[string]interface{}{
		"field_set_key": "profile_grid_item",
		"is_mobile_fork": true,
		"username":      username,
		"slug":          slug,
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

	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", csrftoken))
	}

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
				ID       string `json:"id"`
				Name     string `json:"name"`
				URL      string `json:"url"`
				PinCount int    `json:"pin_count"`
				ImageCoverURL string `json:"image_cover_url"`
				Description string `json:"description"`
				Owner struct {
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
		"add_vase":            true,
		"board_id":            boardID,
		"field_set_key":       "react_grid_pin",
		"filter_section_pins": false,
		"is_react":            true,
		"prepend":             false,
		"page_size":           15,
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

	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("cookie", fmt.Sprintf("csrftoken=%s", csrftoken))
	}

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
				ID     string `json:"id"`
				Type   string `json:"type"`
				Images struct {
					Orig struct {
						URL string `json:"url"`
					} `json:"orig"`
					Size736x struct {
						URL string `json:"url"`
					} `json:"736x"`
					Size474x struct {
						URL string `json:"url"`
					} `json:"474x"`
					Size564x struct {
						URL string `json:"url"`
					} `json:"564x"`
					Size236x struct {
						URL string `json:"url"`
					} `json:"236x"`
				} `json:"images"`
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

		var imageURL string
		if result.Images.Orig.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Orig.URL))
		} else if result.Images.Size736x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size736x.URL))
		} else if result.Images.Size564x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size564x.URL))
		} else if result.Images.Size474x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size474x.URL))
		} else if result.Images.Size236x.URL != "" {
			imageURL = fmt.Sprintf("/image?url=%s", url.QueryEscape(result.Images.Size236x.URL))
		}

		if imageURL != "" {
			pins = append(pins, Pin{
				ID:         result.ID,
				ImageURL:   imageURL,
				PinnerName: strings.TrimSpace(result.Pinner.FullName),
			})
		}
	}

	return pins, responseData.ResourceResponse.Bookmark, newToken
}

func fetchCSRFToken() string {
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

	imageSrc, err := fetchImage(imageUrl)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch image"})
		return
	}

	c.Header("Content-Type", "image/png")
	c.Data(http.StatusOK, "image/png", imageSrc)
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

func fetchImage(imageUrl string) ([]byte, error) {
	resp, err := safeClient.Get(imageUrl)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch image")
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
