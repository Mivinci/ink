package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mivinci/lru"
	"github.com/mivinci/mux"
	"github.com/russross/blackfriday"
	"gopkg.in/yaml.v2"
)

type Option struct {
	Author string
	Brand  string
	Quote  string
	GitHub string
	Since  string
	Cache  int
}

func (o *Option) Load(path string) error {
	buf, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(buf, o)
}

func MustOpt() *Option {
	opt := &Option{}
	if err := opt.Load("ink.yml"); err != nil {
		panic(err)
	}
	return opt
}

func MustWC() *fsnotify.Watcher {
	wc, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	return wc
}

type Post struct {
	Path     string
	Title    string
	Category string
	Size     int64
	HTML     template.HTML
	Time     time.Time
	IsDir    bool
}

func (p *Post) Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Grow(int(fi.Size() + 512)) // just in case
	if _, err = buf.ReadFrom(f); err != nil {
		return err
	}
	pdir, title := parse(path)
	p.Title = title
	p.Category = pdir
	p.Path = path
	p.HTML = template.HTML(blackfriday.MarkdownCommon(buf.Bytes()))
	p.Time = fi.ModTime()
	p.Size = fi.Size()
	return nil
}

type Category struct {
	Name string
	Path string
}

type Dirs struct {
	mu    sync.RWMutex
	cache []*Category
}

func (ds *Dirs) List() []*Category {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.cache
}

func (ds *Dirs) Load(root string) error {
	fis, err := ioutil.ReadDir(root)
	if err != nil {
		return err
	}
	ds.mu.Lock()
	ds.cache = nil // give it to GC
	ds.cache = make([]*Category, 0)
	for _, fi := range fis {
		if fi.IsDir() {
			ds.cache = append(ds.cache, &Category{
				Name: filepath.Base(fi.Name()),
				Path: fi.Name(),
			})
		}
	}
	ds.mu.Unlock()
	return nil
}

type MD struct {
	mu   sync.RWMutex
	fw   *fsnotify.Watcher
	Opt  *Option
	root string
	ext  string

	cache *lru.Cache
	dirs  *Dirs
}

func New(root, ext string) *MD {
	md := &MD{
		root: root,
		ext:  ext,
		dirs: &Dirs{},
		Opt:  MustOpt(),
		fw:   MustWC(),
	}
	md.cache = lru.New(md.Opt.Cache)
	md.cache.Evict = func(k, v interface{}) {}
	must(md.watch())
	must(md.dirs.Load(root))
	return md
}

func (m *MD) watch() error {
	if err := m.fw.Add(m.root); err != nil {
		return err
	}
	log.Printf("(watching) %s\n", m.root)
	return dfs(m.root, func(path string, fi os.FileInfo) error {
		if fi.IsDir() {
			if err := m.fw.Add(path); err != nil {
				return err
			}
			log.Printf("(watching) %s\n", path)
		}
		return nil
	})
}

// Watch must be executed in a goroutine
func (m *MD) Watch() {
	for evt := range m.fw.Events {
		switch evt.Op {
		case fsnotify.Write, fsnotify.Create:
			if m.Is(evt.Name) {
				m.Update(evt.Name) // nolint:errcheck
			}
			m.fw.Add(evt.Name)  // nolint:errcheck
			m.dirs.Load(m.root) // nolint:errcheck
			log.Printf("%s\n", evt.String())
		case fsnotify.Remove, fsnotify.Rename:
			if m.Is(evt.Name) {
				m.Remove(evt.Name)
			}
			m.fw.Remove(evt.Name) // nolint:errcheck
			m.dirs.Load(m.root)   // nolint:errcheck
			log.Printf("%s\n", evt.String())
		case fsnotify.Chmod:
			log.Printf("%s\n", evt.String())
		}
	}
}

func (m *MD) Close() error {
	return m.fw.Close()
}

func (m *MD) Post(path string) (*Post, error) {
	m.mu.RLock()
	p, ok := m.cache.Get(path)
	if ok {
		m.mu.RUnlock()
		log.Printf("%s (hit cache)\n", path)
		return p.(*Post), nil
	}
	m.mu.RUnlock()
	post := &Post{}
	if err := post.Load(path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cache.Add(path, post)
	m.mu.Unlock()
	return post, nil
}

func (m *MD) Update(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.cache.Get(path)
	if !ok { // 缓存里没有，就不更新
		return nil
	}
	post := p.(*Post)
	return post.Load(path)
}

func (m *MD) Remove(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache.Remove(path)
}

func (m *MD) List(dir string) (ps []*Post, err error) {
	ps = make([]*Post, 0)
	err = dfs(dir, func(path string, fi os.FileInfo) error {
		if fi.IsDir() || m.Is(path) {
			p := &Post{
				Path:  m.Clean(path),
				Title: filepath.Base(path),
				Time:  fi.ModTime(),
				Size:  fi.Size(),
			}
			if fi.IsDir() {
				p.IsDir = true
			}
			ps = append(ps, p)
		}
		return nil
	})
	return
}

func (m *MD) Hot() (ps []*Post, err error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ps = make([]*Post, 0)
	err = m.cache.Walk(func(k, v interface{}) error {
		path := k.(string)
		pdir, title := parse(path)
		if title == "index" {
			return nil
		}
		ps = append(ps, &Post{
			Path:     m.Clean(path),
			Title:    title,
			Category: pdir,
			Time:     v.(*Post).Time,
		})
		return nil
	})
	return
}

func (m *MD) Is(path string) bool {
	return filepath.Ext(path) == m.ext
}

func (m *MD) Clean(path string) string {
	return path[len(m.root)+1:]
}

func dfs(root string, fn func(path string, fi os.FileInfo) error) error {
	fis, err := ioutil.ReadDir(root)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		path := filepath.Join(root, fi.Name())
		if err := fn(path, fi); err != nil {
			return err
		}
		if fi.IsDir() {
			if err := dfs(path, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// parse 从path中解析出分类名和文件名
// path不能以 / 开头, 如:
// md/a/b.md -> a, b
// md/b.md ->  , b
func parse(path string) (string, string) {
	j, k, l := 0, 0, 0
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && j == 0 {
			j = i + 1
		} else if path[i] == '/' && j != 0 {
			k = i
		} else if path[i] == '.' {
			l = i
		}
	}
	if k == 0 {
		return "", path[j:l]
	}
	return path[j:k], path[k+1 : l]
}

func isMD(uri string) bool {
	return filepath.Ext(uri) == ".md"
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func htmls(root string) []string {
	ps := make([]string, 0)
	dfs(root, func(path string, fi os.FileInfo) error { // nolint:errcheck
		if filepath.Ext(fi.Name()) == ".html" {
			ps = append(ps, path)
			// log.Printf("%s (template)\n", path)
		}
		return nil
	})
	return ps
}

// shutdown must be executed in a goroutine
func shutdown(fn func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	fn()
}

type Server struct {
	handler *mux.Mux
	server  *http.Server
	tpl     *template.Template
	root    string
	assets  string
}

func NewServer(root, assets string) *Server {
	return &Server{
		handler: mux.New(),
		root:    root,
		assets:  assets,
		tpl:     newTemplate(root),
	}
}

func newTemplate(root string) *template.Template {
	t := template.New("ink").Funcs(funcMap)
	var err error
	if t, err = t.ParseFiles(htmls(root)...); err != nil {
		log.Fatalf("parse templates failed: %s\n", err)
	}
	return t
}

var funcMap = template.FuncMap{
	"upper":     strings.ToUpper,
	"lower":     strings.ToLower,
	"trimLeft":  strings.TrimLeft,
	"trimRight": strings.TrimRight,
	"clean":     clean,
	"size":      size,
}

func clean(path string) string {
	return path[:(len(path) - 3)]
}

func size(i int64) string {
	if i < 1024 {
		return fmt.Sprintf("%dB", i)
	}
	return fmt.Sprintf("%.1fkB", float64(i)/1024)
}

func (s *Server) Start(addr string) {
	s.server = &http.Server{
		Handler: s.handler,
		Addr:    addr,
	}
	log.Println(s.server.ListenAndServe())
}

func (s *Server) Close() error {
	return s.server.Close()
}

func (s *Server) Handle(md *MD) {
	s.handler.Handle("GET", "/static/*", http.FileServer(http.Dir(s.root)))
	s.handler.Handle("GET", "/assets/*", http.FileServer(http.Dir(s.assets)))
	s.handler.HandleFunc("GET", "/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(s.root, "static/favicon.ico"))
	})
	s.handler.HandleFunc("GET", "/", func(w http.ResponseWriter, r *http.Request) {
		ps, err := md.Hot()
		if err != nil {
			log.Printf("get hot posts failed: %s (skipped)\n", err)
		}
		s.tpl.ExecuteTemplate(w, "index", &ListView{ // nolint:errcheck
			Option:     md.Opt,
			List:       ps,
			Count:      len(ps),
			Title:      md.Opt.Brand,
			Categories: md.dirs.List(),
		})
	})
	s.handler.HandleFunc("GET", "/*", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(md.root, r.URL.Path)
		isDir := !isMD(path)
		dir := path
		dirname := md.Clean(dir)

		// 分类
		if isDir {
			path = filepath.Join(path, "index.md")
		}

		// 文章
		post, err := md.Post(path)

		if err != nil {
			// 是分类且用户没有提供分类的自定义目录
			if isDir {
				list, err := md.List(dir)
				if err == nil {
					s.tpl.ExecuteTemplate(w, "posts", &ListView{ // nolint:errcheck
						Option: md.Opt,
						List:   list,
						Title:  dirname,
						Count:  len(list),
					})
					return
				}
			}
			s.tpl.ExecuteTemplate(w, "404", nil) // nolint:errcheck
			return
		}
		if isDir {
			post.Title = dirname
		}
		s.tpl.ExecuteTemplate(w, "post", &PostView{Option: md.Opt, Post: post}) // nolint:errcheck
	})
}

type PostView struct {
	*Option
	*Post
}

type ListView struct {
	*Option
	Title string
	List  []*Post
	Count int

	Categories []*Category
}

var (
	port   int
	md     string
	html   string
	assets string
)

func init() {
	flag.IntVar(&port, "p", 8080, "port to listen")
	flag.StringVar(&md, "md", "md", "markdown filepath")
	flag.StringVar(&html, "html", "html", "html template filepath")
	flag.StringVar(&assets, "assets", ".", "assets filepath")
}

func main() {
	flag.Parse()

	m := New(md, ".md")
	go m.Watch()

	s := NewServer(html, assets)
	s.Handle(m)

	go shutdown(func() {
		m.Close()
		log.Println("ink: closed")
		s.Close()
	})

	s.Start(fmt.Sprintf(":%d", port))
}
