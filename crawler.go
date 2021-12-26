package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/mux"
	"github.com/headzoo/surf/browser"
	"gopkg.in/headzoo/surf.v1"
)

type Article struct {
	Title   string     `json:"title"`
	Uri     string     `json:"uri"`
	Content string     `json:"content"`
	Links   []LinkItem `json:"links"`
}

type Page struct {
	Uri      string     `json:"uri"`
	Exists   bool       `json:"exists"`
	Cached   bool       `json:"cached"`
	Title    string     `json:"title"`
	Articles []Article  `json:"articles"`
	Links    []LinkItem `json:"links"`
}

func (p *Page) setCached() {
	p.Cached = true
}

type CountItem struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

type PageStats struct {
	Uri    string      `json:"uri"`
	Exists bool        `json:"exists"`
	Counts []CountItem `json:"counts"`
	Words  []string    `json:"words"`
}

func newPageStats(uri string, exists bool) PageStats {
	var counts []CountItem
	var words []string
	return PageStats{Uri: uri, Exists: exists, Counts: counts, Words: words}
}

func (ps *PageStats) addCountItem(key string, val int) PageStats {
	ci := CountItem{Key: key, Value: val}
	ps.Counts = append(ps.Counts, ci)
	return *ps
}

func (ps *PageStats) setWords(words []string) PageStats {
	ps.Words = words
	return *ps
}

type ClassesIdSet struct {
	ParentPath string   `json:"parentPath"`
	TagName    string   `json:"tagName"`
	Id         string   `json:"id"`
	Classes    []string `json:"classes"`
	WordCount  int      `json:"wordCount"`
}

func extractClasses(selection *goquery.Selection) []string {
	val, exists := selection.Attr("class")
	classList := []string{}
	if exists {
		classList = strings.Split(val, " ")
	}
	return classList
}

func buildClassesIdSet(selection *goquery.Selection) ClassesIdSet {
	val, exists := selection.Attr("id")
	id := ""
	if exists {
		id = val
	}
	classes := extractClasses(selection)
	wordCount := extractNumWords(selection)
	tagName := goquery.NodeName(selection)
	parent := selection.Parent()
	parentPath := ""
	if parent.Length() > 0 {
		parentSet := buildClassesIdSet(parent)
		if parentSet.TagName != "body" && parentSet.TagName != "html" {
			parentPath = parentSet.ToPath()
		}
		if !strings.Contains(parentPath, ".") && !strings.Contains(parentPath, "#") {
			parent = parent.Parent()
			if parent.Length() > 0 {
				parentSet = buildClassesIdSet(parent)
				if parentSet.TagName != "body" && parentSet.TagName != "html" {
					parentPath = parentSet.ToPath()
				}
			}
		}
	}
	return ClassesIdSet{Id: id, Classes: classes, WordCount: wordCount, TagName: tagName, ParentPath: parentPath}
}

func (cs *ClassesIdSet) ToPath() string {
	parts := []string{cs.TagName}
	if len(cs.Id) > 0 {
		parts = append(parts, "#"+cs.Id)
	}
	if len(cs.Classes) > 0 {
		parts = append(parts, "."+strings.Join(cs.Classes, "."))
	}
	return strings.Trim(strings.Join([]string{cs.ParentPath, strings.Join(parts, "")}, " "), " ")
}

type LinkItem struct {
	Title string `json:"title"`
	Uri   string `json:"uri"`
}

func uriIsInLinkItems(links []LinkItem, str string) bool {
	for i := 0; i < len(links); i++ {
		if links[i].Uri == str {
			return true
		}
	}
	return false
}

func makeArticle(title string, uri string, content string, links []LinkItem) Article {
	return Article{Title: title, Uri: uri, Content: content, Links: links}
}

func makePage(title string, uri string, exists bool, articles []Article, links []LinkItem) Page {
	return Page{Title: title, Uri: uri, Exists: exists, Articles: articles, Links: links, Cached: false}
}

func emptyPage() Page {
	var articles []Article
	var links []LinkItem
	return Page{Title: "", Uri: "", Exists: false, Articles: articles, Links: links, Cached: false}
}

func main() {
	handleRequests()
}

func homePage(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	useCache := vars["cacheMode"] != "refresh"
	page, isCached := readBlogPage(vars["url"], vars["scheme"], useCache)
	cacheType := "-"
	if isCached {
		cacheType = "redis"
	}
	w.Header().Set("cached", cacheType)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	json.NewEncoder(w).Encode(page)
}

func infoJson(w http.ResponseWriter, r *http.Request) {
	routes := [2]string{"/", "/blog/:uri/:scheme/:cacheMode"}
	data := map[string]interface{}{
		"title":  "Welcome",
		"routes": routes,
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	json.NewEncoder(w).Encode(data)
}

func handleRequests() {
	myRouter := mux.NewRouter().StrictSlash(true)
	myRouter.HandleFunc("/", infoJson)
	myRouter.HandleFunc("/info", infoJson)
	myRouter.HandleFunc("/blog/{url}/{scheme}/{cacheMode}", homePage)
	myRouter.HandleFunc("/discover/{url}/{scheme}", discoverPage)
	log.Fatal(http.ListenAndServe(":3756", myRouter))
}

func storeClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "", // no password set
		DB:       0,  // use default DB
	})
}

func setCache(key string, data interface{}, minutes int64) bool {
	var ctx = context.Background()
	rdb := storeClient()
	duration := time.Duration(minutes) * time.Minute
	ret, err := json.MarshalIndent(data, "", " ")
	rdb.Set(ctx, key, ret, duration)
	return err == nil
}

func getCache(key string) (result interface{}, errVal error) {
	var ctx = context.Background()
	rdb := storeClient()
	var page = emptyPage()
	val, err := rdb.Get(ctx, key).Result()
	if err == nil {
		json.Unmarshal([]byte(val), &page)
	}
	result = page
	errVal = err
	return
}

func readBlogPage(path string, scheme string, cached bool) (page Page, isCached bool) {
	uri := scheme + "://" + path
	cacheKey := "page:" + path
	result, errVal := getCache(cacheKey)
	if errVal == nil && cached {
		page = result.(Page)
		page.setCached()
		isCached = true
		return
	} else {
		data := readLiveBlogPage(uri)
		setCache(cacheKey, data, 1440)
		page = data
		isCached = false
		return
	}
}

func readLiveBlogPage(uri string) Page {
	bow := surf.NewBrowser()
	err := bow.Open(uri)
	exists := err == nil
	title := ""
	var links []LinkItem
	var articles []Article
	if exists {
		articles = readBlogArticles(bow)
		linkObjs := bow.Links()
		title = bow.Title()
		for i := 0; i < len(linkObjs); i++ {
			linkRef := linkObjs[i]
			path := linkRef.Url().Path
			if len(path) > 0 {
				newLink := LinkItem{Uri: path, Title: linkRef.Text}
				if !uriIsInLinkItems(links, path) {
					links = append(links, newLink)
				}
			}
		}
	}
	return makePage(title, uri, exists, articles, links)
}

func removeSpaces(text string) string {
	cleanSpaceRgx := regexp.MustCompile(`\s\s+`)
	return cleanSpaceRgx.ReplaceAllString(strings.Trim(text, " "), " ")
}

func extractWords(selection *goquery.Selection) []string {
	text := removeSpaces(selection.Text())
	return strings.Split(text, " ")
}

func hasTextNodes(selection *goquery.Selection) bool {
	nodes := selection.Nodes
	for i := 0; i < len(nodes); i++ {
		node := nodes[i]
		switch int(node.Type) {
		case 1:
			hasSelection := len(removeSpaces(node.Data)) > 2
			if hasSelection {
				return true
			}
		}
	}
	return false
}

func extractNumWords(selection *goquery.Selection) int {
	return len(extractWords(selection))
}

func discoverPage(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	url := vars["scheme"] + "://" + vars["url"]
	ps := discoverLivePage(url)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	json.NewEncoder(w).Encode(ps)
}

func discoverLivePage(uri string) PageStats {
	bow := surf.NewBrowser()
	err := bow.Open(uri)
	exists := err == nil

	ps := newPageStats(uri, exists)
	if exists {
		ps.addCountItem("links", len(bow.Links()))
		ps.addCountItem("articleTags", bow.Find("article").Length())
		ps.addCountItem("sectionTags", bow.Find("section").Length())
		ps.addCountItem("tableTags", bow.Find("table").Length())
		body := bow.Find("body")
		body.Find("img,figure,object,iframe,svg,audio,video,script,style").Remove()
		ps.addCountItem("words", extractNumWords(body))
		ps.addCountItem("numInnerLinks", body.Find("a").Length())
		body.Find("a").Remove()
		ps.addCountItem("wordsNotInLinks", extractNumWords(body))
		tags := body.Find("div, article, section, aside")
		/* for i := 0; i < tags.Length(); i++ {
			if hasTextNodes(tags.Eq(i)) {
				currEl := tags.Eq(i).Clone()
				currEl.Find("a").Remove()
				numWs := extractNumWords(currEl)
				if numWs > 5 {
					numWords += numWs
				}
			}
		} */
		for i := 0; i < tags.Length(); i++ {
			cData := buildClassesIdSet(tags.Eq(i))
			if cData.WordCount > 16 {
				ps.addCountItem(cData.ToPath(), cData.WordCount)
			}
		}
	}
	return ps
}

func readBlogArticles(bow *browser.Browser) []Article {
	var articles = bow.Find("article")
	const maxNum = 100
	p1 := regexp.MustCompile(`<!--[^>]*?-->`)
	articles.Find("img,svg,embed,iframe,object,style,script").Remove()
	numArticles := articles.Length()
	var output [maxNum]Article
	for i := 0; i < numArticles; i++ {
		if i < maxNum {

			itemHtml, itemErr := articles.Eq(i).Html()
			if itemErr == nil {
				content := strings.Trim(p1.ReplaceAllString(itemHtml, ""), "\n\t ")
				titleEls := articles.Eq(i).Find("h1,h2,h3")
				if titleEls.Length() > 0 {
					titleElement := titleEls.First()
					title := titleElement.Text()
					linkEl := titleElement.Find("a")
					if linkEl.Length() > 0 {
						uri := linkEl.AttrOr("href", "")
						linkEls := articles.Eq(i).Find("a")
						numLinks := linkEls.Length()
						var links []LinkItem
						for j := 0; j < numLinks; j++ {
							val, exists := linkEls.Eq(j).Attr("href")
							if exists {
								lk := LinkItem{Uri: val, Title: linkEls.Eq(j).Text()}
								if !uriIsInLinkItems(links, val) {
									links = append(links, lk)
								}
							}
						}
						output[i] = makeArticle(title, uri, content, links)
					}
				}
			}
		}
	}
	return output[0:numArticles]
}
