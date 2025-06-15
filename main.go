package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/robfig/cron/v3"
	"go.etcd.io/bbolt" // 或 "github.com/dgraph-io/badger"
)

// main.go 顶部添加
import "os"

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func getEnvInt(key string, defaultValue int) int {
	if val := os.Getenv(key); val != "" {
		if intVal, err := strconv.Atoi(val); err == nil {
			return intVal
		}
	}
	return defaultValue
}

// 主函数前定义配置变量
var (
	BASE_URL     = getEnv("BASE_URL", "https://javdb.com")
	QB_URL       = getEnv("QB_URL", "http://127.0.0.1:8080")
	QB_USERNAME  = getEnv("QB_USERNAME", "admin")
	QB_PASSWORD  = getEnv("QB_PASSWORD", "adminadmin")
	USER_AGENT   = getEnv("USER_AGENT", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	WORKER_CNT   = getEnvInt("WORKER_COUNT", 1)
	MAX_DEPTH    = getEnvInt("MAX_DEPTH", 3)
	RUN_SCHEDULE = getEnv("RUN_SCHEDULE", "") // 定时任务配置，为空则立即运行
	START_URL    = getEnv("START_URL", "/rankings/movies?p=daily&t=censored")
)

// 爬虫结果
type ScrapeResult struct {
	URL   string
	Data  interface{}
	Error error
}

// 爬虫请求
type Request struct {
	URL       string
	Ctx       context.Context
	Depth     int
	ParseFunc func(context.Context, *goquery.Document) ([]Request, interface{})
}

// 爬虫框架核心
type Crawler struct {
	MaxDepth   int
	Workers    int
	UserAgent  string
	Delay      time.Duration
	OnResult   func(*ScrapeResult)
	seenURLs   map[string]struct{}
	seenMutex  sync.RWMutex
	requestCh  chan Request
	results    chan ScrapeResult
	wg         sync.WaitGroup
	cancelFunc context.CancelFunc
	idleWg     sync.WaitGroup
	activeReqs int
	activeMu   sync.Mutex
	done       chan struct{} // 用于通知爬虫已完成的通道
}

// 创建新爬虫实例
func NewCrawler() *Crawler {
	return &Crawler{
		MaxDepth:  MAX_DEPTH,
		Workers:   WORKER_CNT,
		UserAgent: USER_AGENT,
		Delay:     1 * time.Second,
		seenURLs:  make(map[string]struct{}),
		done:      make(chan struct{}),
	}
}

// 启动爬虫
func (c *Crawler) Run(startURL string, parseFunc func(ctx context.Context, doc *goquery.Document) ([]Request, interface{})) {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancelFunc = cancel

	// 初始化通道
	c.requestCh = make(chan Request, 100)
	c.results = make(chan ScrapeResult, 50)

	// 启动工作者
	for i := 0; i < c.Workers; i++ {
		c.wg.Add(1)
		go c.worker()
	}

	// 启动结果处理器
	go c.processResults()

	// 启动监控器（当队列为空时停止）
	go c.monitorQueue()

	// 提交初始请求
	c.idleWg.Add(1)
	c.submitRequest(Request{
		URL:       startURL,
		Ctx:       ctx,
		Depth:     0,
		ParseFunc: parseFunc,
	})
}

// 等待爬虫完成
func (c *Crawler) Wait() {
	<-c.done // 等待完成信号
}

// 停止爬虫
func (c *Crawler) Stop() {
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	close(c.requestCh)
	c.wg.Wait()
	close(c.results)
	close(c.done) // 通知完成
}

// 提交请求（带去重检查）
func (c *Crawler) submitRequest(req Request) {
	c.seenMutex.Lock()
	defer c.seenMutex.Unlock()

	// 规范化 URL
	u, err := url.Parse(req.URL)
	if err != nil {
		return
	}
	u.Fragment = "" // 移除锚点
	key := u.String()

	// 检查是否已访问
	if _, exists := c.seenURLs[key]; exists {
		log.Printf("跳过已访问的URL: %s", key)
		return
	}
	c.seenURLs[key] = struct{}{} // 标记为已访问

	// 提交到队列
	select {
	case c.requestCh <- req:
		c.activeMu.Lock()
		c.activeReqs++ // 增加活跃请求计数
		c.activeMu.Unlock()
	default:
		log.Printf("请求队列已满，忽略请求: %s", key)
	}
}

// 工作协程
func (c *Crawler) worker() {
	defer c.wg.Done()

	for req := range c.requestCh {
		select {
		case <-req.Ctx.Done():
			return
		default:
			c.activeMu.Lock()
			c.activeReqs--
			c.activeMu.Unlock()

			res, err := c.fetch(req)
			c.results <- ScrapeResult{
				URL:   req.URL,
				Data:  res,
				Error: err,
			}
			time.Sleep(c.Delay) // 控制请求频率
		}
	}
}

// 执行 HTTP 请求和解析
func (c *Crawler) fetch(req Request) (interface{}, error) {
	log.Printf("爬取: %s (深度: %d)", req.URL, req.Depth)

	// 创建 HTTP 客户端
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// 创建请求
	httpReq, err := http.NewRequestWithContext(req.Ctx, "GET", req.URL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("User-Agent", c.UserAgent)

	// 发送请求
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP错误: %d", resp.StatusCode)
	}

	// 解析 HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	// 执行用户定义的解析函数
	newReqs, data := req.ParseFunc(req.Ctx, doc)

	// 提交新请求（深度控制）
	if len(newReqs) > 0 {
		for _, nr := range newReqs {
			nr.Depth = req.Depth + 1
			nr.Ctx = req.Ctx

			if nr.Depth < c.MaxDepth {
				c.idleWg.Add(1)
				c.submitRequest(nr)
			}
		}
	}

	// 标记当前请求完成
	c.idleWg.Done()

	return data, nil
}

// 处理结果
func (c *Crawler) processResults() {
	for res := range c.results {
		if c.OnResult != nil {
			c.OnResult(&res)
		}
	}
}

// 监控队列状态
func (c *Crawler) monitorQueue() {
	for {
		// 短暂延迟后再检查
		time.Sleep(1 * time.Second)

		c.activeMu.Lock()
		activeReqs := c.activeReqs
		c.activeMu.Unlock()

		log.Printf("监控: 活跃请求 = %d, idleWg = %d", activeReqs, c.idleWg)

		// 等待所有请求处理完成
		if activeReqs == 0 {
			if waitWithTimeout(&c.idleWg, 3*time.Second) {
				log.Println("检测到队列空闲且所有请求已完成")
				c.Stop()
				return
			}
		}
	}
}

// 带超时的WaitGroup等待
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return true
	case <-time.After(timeout):
		return false
	}
}

// 列表页解析函数
func listParser(ctx context.Context, doc *goquery.Document) ([]Request, interface{}) {
	var movieURLs []Request

	// 查找所有电影项 - 根据实际HTML结构调整选择器
	doc.Find(".movie-list .item").Each(func(i int, s *goquery.Selection) {
		// 提取电影URL
		moviePath, exists := s.Find("a").Attr("href")
		if exists {
			movieURL := BASE_URL + moviePath
			movieURLs = append(movieURLs, Request{
				URL:       movieURL,
				ParseFunc: movieDetailParser,
			})
			log.Printf("发现电影链接: %s", movieURL)
		}
	})

	return movieURLs, "列表页面解析完成"
}

// 解析新的HTML结构
func parseNewHTML(doc *goquery.Document) []MagnetItem {
	var items []MagnetItem

	// 解析每个磁力链接项
	doc.Find(".item").Each(func(i int, s *goquery.Selection) {
		// 提取名称
		name := s.Find(".name").First().Text()
		if name == "" {
			return
		}

		// 提取日期
		dateStr := s.Find(".time").First().Text()
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			log.Printf("解析日期失败: %s", dateStr)
			return
		}

		// 提取磁力链接 - 从复制按钮获取
		magnet := ""
		s.Find(".copy-to-clipboard").Each(func(j int, btn *goquery.Selection) {
			if m, exists := btn.Attr("data-clipboard-text"); exists {
				magnet = m
			}
		})

		if magnet == "" {
			log.Printf("未找到磁力链接: %s", name)
			return
		}

		// 检查是否有字幕
		hasSub := false
		s.Find(".tag").Each(func(k int, tag *goquery.Selection) {
			if strings.Contains(tag.Text(), "字幕") {
				hasSub = true
			}
		})

		// 检查是否高清
		isHD := false
		s.Find(".tag").Each(func(k int, tag *goquery.Selection) {
			if strings.Contains(tag.Text(), "高清") {
				isHD = true
			}
		})

		item := MagnetItem{
			Name:   name,
			Date:   date,
			Magnet: magnet,
			HasSub: hasSub,
			IsHD:   isHD,
		}

		items = append(items, item)
	})

	return items
}

func sortTorrentItems(items []MagnetItem) {
	sort.Slice(items, func(i, j int) bool {
		// 优先高清且有字幕的
		iHDSub := items[i].IsHD && items[i].HasSub
		jHDSub := items[j].IsHD && items[j].HasSub
		if iHDSub && !jHDSub {
			return true
		} else if !iHDSub && jHDSub {
			return false
		}

		// 次选高清或有字幕的
		iHasFeature := items[i].IsHD || items[i].HasSub
		jHasFeature := items[j].IsHD || items[j].HasSub
		if iHasFeature && !jHasFeature {
			return true
		} else if !iHasFeature && jHasFeature {
			return false
		}

		// 日期最新的优先
		if items[i].Date.After(items[j].Date) {
			return true
		} else if items[i].Date.Before(items[j].Date) {
			return false
		}

		// 其他情况按名称排序
		return items[i].Name < items[j].Name
	})
}

// qBittorrent相关函数
func addToQbittorrent(magnet string) error {
	// 1. 登录获取cookie
	cookie, err := qbLogin()
	if err != nil {
		return fmt.Errorf("登录失败: %w", err)
	}

	// 2. 添加磁力链接到下载队列
	if err := qbAddTorrent(magnet, cookie); err != nil {
		return fmt.Errorf("添加磁力链接失败: %w", err)
	}

	return nil
}

func qbLogin() (*http.Cookie, error) {
	loginURL := fmt.Sprintf("%s/api/v2/auth/login", QB_URL)

	// 创建登录表单
	form := url.Values{}
	form.Add("username", QB_USERNAME)
	form.Add("password", QB_PASSWORD)

	// 发送登录请求
	resp, err := http.PostForm(loginURL, form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查登录是否成功
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("登录失败: %s, 响应: %s", resp.Status, string(body))
	}

	// 获取登录cookie
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return nil, fmt.Errorf("未获取到认证cookie")
	}

	return cookies[0], nil
}

func qbAddTorrent(magnet string, cookie *http.Cookie) error {
	addURL := fmt.Sprintf("%s/api/v2/torrents/add", QB_URL)

	// 创建请求参数
	params := url.Values{}
	params.Add("urls", magnet)
	params.Add("autoTMM", "false")
	params.Add("savepath", "") // 使用默认保存路径
	params.Add("category", "") // 不设置分类

	// 创建请求
	req, err := http.NewRequest("POST", addURL, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}

	// 设置Cookie和请求头
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", QB_URL)

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 检查响应
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API返回错误: %s, 响应: %s", resp.Status, string(body))
	}

	return nil
}

// 存储解析结果
type MagnetItem struct {
	Name     string
	Date     time.Time
	Magnet   string
	HasSub   bool
	IsHD     bool
	FileInfo string
}

// 记录排序过程
func logSortProcess(title string, items []MagnetItem) {
	if len(items) == 0 {
		return
	}

	log.Printf("电影 '%s' 的磁力链接排序过程:", title)
	log.Printf("找到 %d 个磁力链接:", len(items))

	for i, item := range items {
		log.Printf("%d. %s (日期: %s, 字幕: %t, 高清: %t)",
			i+1, item.Name, item.Date.Format("2006-01-02"), item.HasSub, item.IsHD)
	}

	best := items[0]
	log.Printf("选择最佳磁力链接: %s (理由: %s)", best.Name, getSelectionReason(best, items))
}

// 获取选择理由
func getSelectionReason(selected MagnetItem, items []MagnetItem) string {
	if len(items) == 1 {
		return "唯一选项"
	}

	reasons := []string{}

	// 检查是否有最新
	isNewest := true
	for _, item := range items {
		if item.Date.After(selected.Date) {
			isNewest = false
			break
		}
	}
	if isNewest {
		reasons = append(reasons, "最新")
	}

	// 检查是否有字幕
	if selected.HasSub {
		reasons = append(reasons, "有字幕")
	}

	// 检查是否高清
	if selected.IsHD {
		reasons = append(reasons, "高清")
	}

	// 检查是否最高优先级
	if len(reasons) == 0 {
		return "默认优先级最高"
	}

	return strings.Join(reasons, ", ")
}

// 使用BoltDB实现数据库
type DownloadDB struct {
	db *bbolt.DB
}

func (d *DownloadDB) Open() error {
	db, err := bbolt.Open("downloads.db", 0600, nil)
	if err != nil {
		return err
	}
	d.db = db
	return d.db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("downloaded_movies"))
		return err
	})
}

func (d *DownloadDB) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

func (d *DownloadDB) IsDownloaded(title string) (bool, error) {
	found := false
	err := d.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte("downloaded_movies"))
		if bucket == nil {
			return nil
		}
		v := bucket.Get([]byte(title))
		found = v != nil
		return nil
	})
	return found, err
}

func (d *DownloadDB) AddDownload(title, magnet string) error {
	return d.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("downloaded_movies"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(title), []byte(magnet))
	})
}

var downloadDB DownloadDB

func init() {
	// 初始化数据库
	if err := downloadDB.Open(); err != nil {
		log.Fatalf("无法打开数据库: %v", err)
	}
}
func main() {
	defer func() {
		if err := downloadDB.Close(); err != nil {
			log.Printf("关闭数据库错误: %v", err)
		}
	}()

	log.Println("爬虫程序启动")

	// 创建执行函数
	runCrawler := func() {
		stats := &Stats{}
		crawler := NewCrawler()

		crawler.OnResult = func(res *ScrapeResult) {
			// ... 原有结果处理逻辑 ...
		}

		// 启动爬虫
		url := BASE_URL + START_URL
		crawler.Run(url, listParser)

		// 等待爬虫完成
		log.Println("等待爬虫完成工作...")
		crawler.Wait()
		log.Println("爬虫工作已完成")

		// 打印统计报告
		printStats(stats)
	}

	// 检查是否需要定时执行
	if RUN_SCHEDULE != "" {
		log.Printf("配置定时任务，计划: %s", RUN_SCHEDULE)

		// 初始化cron调度器
		c := cron.New()
		_, err := c.AddFunc(RUN_SCHEDULE, runCrawler)
		if err != nil {
			log.Fatalf("无效的cron表达式: %s", RUN_SCHEDULE)
		}

		// 首次立即执行
		go runCrawler()

		// 启动调度器并永久运行
		c.Start()
		log.Println("定时任务调度器已启动")

		// 防止程序退出
		select {}
	} else {
		log.Println("立即执行爬虫任务...")
		runCrawler()
		log.Println("爬虫程序结束")
	}
}

// 修改电影详情解析器检查已下载
func movieDetailParser(ctx context.Context, doc *goquery.Document) ([]Request, interface{}) {
	// 提取电影标题
	title, _ := doc.Find(".movie-panel-info .copy-to-clipboard").First().Attr("data-clipboard-text")
	log.Printf("处理电影详情页: %s", title)
	result := MovieResult{Title: title}

	// 检查是否已下载
	if downloaded, err := downloadDB.IsDownloaded(title); err != nil {
		log.Printf("数据库查询失败: %v", err)
		result.DownloadSuccess = false
		return nil, result
	} else if downloaded {
		log.Printf("电影 '%s' 已下载，跳过", title)
		result.SkipReason = "已下载"
		return nil, result
	}

	// 提取磁力链接区域
	magnetNode := doc.Find("#magnets-content").First()
	if magnetNode.Length() == 0 {
		result.TorrentsFound = 0
		result.DownloadSuccess = false
		result.SkipReason = "无磁力区域"
		return nil, result
	}

	// 解析磁力链接
	items := parseNewHTML(doc)
	if len(items) == 0 {
		result.TorrentsFound = 0
		result.DownloadSuccess = false
		result.SkipReason = "无磁力链接"
		return nil, result
	}

	result.Torrents = items
	result.TorrentsFound = len(items)

	// 按优先级排序
	sortTorrentItems(items)

	// 记录排序过程
	logSortProcess(title, items)

	// 选择最佳种子文件磁力链接
	best := items[0]
	result.SelectedName = best.Name

	// 添加到下载队列
	if err := addToQbittorrent(best.Magnet); err != nil {
		result.DownloadSuccess = false
		result.SkipReason = "添加下载失败"
		return nil, result
	}

	// 记录到数据库
	if err := downloadDB.AddDownload(title, best.Magnet); err != nil {
		log.Printf("数据库记录失败: %v", err)
	} else {
		log.Printf("电影 '%s' 已添加到下载记录", title)
	}

	result.DownloadSuccess = true
	return nil, result
}

// 更新MovieResult结构
type MovieResult struct {
	Title           string
	TorrentsFound   int
	Torrents        []MagnetItem
	SelectedName    string
	DownloadSuccess bool
	SkipReason      string // 新增：跳过原因
}

// 打印统计报告
func printStats(stats *Stats) {
	fmt.Println("\n================= 爬虫统计报告 =================")
	fmt.Printf("总请求数: %d\n", stats.totalRequests)
	fmt.Printf("成功请求: %d\n", stats.successful)
	fmt.Printf("错误请求: %d\n", stats.errors)
	fmt.Printf("处理的电影数: %d\n", stats.moviesProcessed)
	fmt.Printf("带磁力链接的电影: %d\n", stats.moviesWithTorrents)
	fmt.Printf("已下载的电影: %d\n", stats.downloadSuccess)
	fmt.Printf("下载添加失败: %d\n", stats.downloadFail)
	fmt.Printf("无磁力链接的电影: %d\n", stats.moviesProcessed-stats.moviesWithTorrents)
	fmt.Printf("跳过已下载的电影: %d\n", stats.skippedMovies) // 需要在stats中添加此字段
	fmt.Println("=============================================")
}

// 更新Stats结构
type Stats struct {
	totalRequests      int
	successful         int
	errors             int
	moviesProcessed    int
	moviesWithTorrents int
	downloadSuccess    int
	downloadFail       int
	skippedMovies      int // 新增：跳过的电影数
}
