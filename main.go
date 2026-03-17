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
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
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
}

type PinData struct {
	Pin     Pin
	Related []Pin
}

var allowedDomains = []string{"pinimg.com", "i.pinimg.com", "pinterest.com"}

func main() {
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	router := gin.Default()

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

	_ = godotenv.Load()
	URL := os.Getenv("URL")
	if URL == "" {
		URL = "http://127.0.0.1:3000"
	}

	fmt.Println(` _____ _     _             
|  _  |_|___| |___ ___ ___ 
|   __| |   | | -_|_ -|_ -|
|__|  |_|_|_|_|___|___|___|
`)
	fmt.Printf("Server running at %s\n\n", URL)

	router.Run(":3000")
}

func searchHandler(c *gin.Context) {
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file")
	}

	URL := os.Getenv("URL")
	if URL == "" {
		return
	}
	query := c.Query("q")

	// get bookmark from cookie for privacy
	bookmark := ""
	if cookie, err := c.Cookie("bookmark"); err == nil && cookie != "" {
		bookmark = cookie
	}

	// clear bookmark if new search
	if _, nextExists := c.GetQuery("next"); !nextExists {
		c.SetCookie("bookmark", "", -1, "/", "", false, true)
		bookmark = ""
	}

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
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
				c.SetCookie("csrftoken", csrftoken, 0, "/", "", false, true)
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
		c.SetCookie("bookmark", responseData.ResourceResponse.Bookmark, 0, "/", "", false, true)
	} else {
		// clear cookie when no pages
		c.SetCookie("bookmark", "", -1, "/", "", false, true)
	}

	type ResultItem struct {
		ID    string
		Image string
	}

	var results []ResultItem
	for _, result := range responseData.ResourceResponse.Data.Results {
		imageUrl := result.Images.Orig.URL
		if imageUrl != "" && isAllowedDomain(imageUrl) {
			proxyImageUrl := fmt.Sprintf("%s/image?url=%s", URL, url.QueryEscape(imageUrl))
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
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file")
	}

	URL := os.Getenv("URL")
	if URL == "" {
		return
	}

	pinID := c.Param("id")
	query := c.Query("q")
	from := c.Query("from")

	bookmark := ""
	if cookie, err := c.Cookie("bookmark"); err == nil && cookie != "" {
		bookmark = cookie
	}

	if _, nextExists := c.GetQuery("next"); !nextExists {
		c.SetCookie("bookmark", "", -1, "/", "", false, true)
		bookmark = ""
	}

	csrftoken := ""
	if cookie, err := c.Cookie("csrftoken"); err == nil && cookie != "" {
		csrftoken = cookie
	}

	pin := fetchPinDetails(pinID, csrftoken, URL)

	related, nextBookmark := fetchRelatedPins(pinID, csrftoken, URL, bookmark)

	if nextBookmark != "" {
		c.SetCookie("bookmark", nextBookmark, 0, "/", "", false, true)
	} else {
		c.SetCookie("bookmark", "", -1, "/", "", false, true)
	}

	c.HTML(http.StatusOK, "pin.html", gin.H{
		"Pin":             pin,
		"Related":         related,
		"RelatedBookmark": nextBookmark,
		"Query":           query,
		"From":            from,
	})
}

func fetchPinDetails(pinID string, csrftoken string, baseURL string) Pin {
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
		Title:       data.Title,
		Description: data.Description,
		PinnerName:  data.Pinner.FullName,
	}

	if data.Images.Orig.URL != "" {
		pin.ImageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(data.Images.Orig.URL))
	} else if data.Images.Size736x.URL != "" {
		pin.ImageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(data.Images.Size736x.URL))
	} else if data.Images.Size564x.URL != "" {
		pin.ImageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(data.Images.Size564x.URL))
	} else if data.Images.Size474x.URL != "" {
		pin.ImageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(data.Images.Size474x.URL))
	} else if data.Images.Size236x.URL != "" {
		pin.ImageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(data.Images.Size236x.URL))
	}

	return pin
}

func fetchRelatedPins(pinID string, csrftoken string, baseURL string, bookmark string) ([]Pin, string) {
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
			imageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(result.Images.Orig.URL))
		} else if result.Images.Size736x.URL != "" {
			imageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(result.Images.Size736x.URL))
		} else if result.Images.Size564x.URL != "" {
			imageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(result.Images.Size564x.URL))
		} else if result.Images.Size474x.URL != "" {
			imageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(result.Images.Size474x.URL))
		} else if result.Images.Size236x.URL != "" {
			imageURL = fmt.Sprintf("%s/image?url=%s", baseURL, url.QueryEscape(result.Images.Size236x.URL))
		}

		if imageURL != "" {
			related = append(related, Pin{
				ID:         result.ID,
				ImageURL:   imageURL,
				Title:      title,
				PinnerName: result.Pinner.FullName,
			})
		}
	}

	return related, responseData.ResourceResponse.Bookmark
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

func fetchImage(imageUrl string) ([]byte, error) {
	resp, err := http.Get(imageUrl)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch image")
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
