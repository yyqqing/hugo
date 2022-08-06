package commands

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/hugolib"
	"github.com/gohugoio/hugo/livereload"
	"github.com/gohugoio/hugo/output"
	"github.com/gohugoio/hugo/resources/page"
	"github.com/gohugoio/hugo/tpl"
	"github.com/gohugoio/hugo/watcher"
	"github.com/spf13/afero"
	jww "github.com/spf13/jwalterweatherman"
)

type HugoServer struct {
	c             *commandeer
	baseURLs      []string
	roots         []string
	ports         []int
	errorTemplate func(err any) (io.Reader, error)

	workingDir string

	Watcher *watcher.Batcher
	Servers []*http.Server
}

func (hs *HugoServer) HugoTry() *hugolib.HugoSites {
	select {
	case <-hs.c.created:
		return hs.c.hugoSites
	case <-time.After(time.Millisecond * 100):
		return nil
	}
}

func (hs *HugoServer) Rebuild() error {
	configMap := map[string]any{}

	cfgInit := func(c *commandeer) error {
		for key, value := range configMap {
			c.Set(key, value)
		}
		return nil
	}

	c, err := initializeConfig(true, true, true, &hugoBuilderCommon{
		source: hs.workingDir,
	}, nil, cfgInit)

	if err != nil {
		log.Println("Error:", err.Error())
		return err
	}

	hs.c = c

	err = func() error {
		defer c.timeTrack(time.Now(), "Built")
		err := c.serverBuild()
		if err != nil {
			log.Println("Error:", err.Error())
		}
		return err
	}()
	if err != nil {
		return err
	}
	return nil
}

func (hs *HugoServer) Config(source string, previewPort int) error {
	configMap := map[string]any{
		"buildExpired":   true,
		"buildDrafts":    true,
		"buildFuture":    true,
		"previewPort":    previewPort,
		"renderToMemory": true,
		"watch":          true,
		"bind":           "127.0.0.1",
	}

	cfgInit := func(c *commandeer) error {
		for key, value := range configMap {
			c.Set(key, value)
		}
		return nil
	}

	c, err := initializeConfig(true, true, true, &hugoBuilderCommon{
		source: source,
	}, nil, cfgInit)

	if err != nil {
		log.Println("Error:", err.Error())
		return err
	}

	hs.workingDir = source
	hs.c = c

	err = func() error {
		defer c.timeTrack(time.Now(), "Built")
		err := c.serverBuild()
		if err != nil {
			log.Println("Error:", err.Error())
		}
		return err
	}()
	if err != nil {
		return err
	}

	// Watch runs its own server as part of the routine
	watchDirs, err := c.getDirList()
	if err != nil {
		return err
	}

	watchGroups := helpers.ExtractAndGroupRootPaths(watchDirs)

	for _, group := range watchGroups {
		jww.FEEDBACK.Printf("Watching for changes in %s\n", group)
	}
	watcher, err := c.newWatcher("", watchDirs...)
	if err != nil {
		return err
	}

	hs.Watcher = watcher
	// defer watcher.Close()

	return hs.setupServers()
}

func (hs *HugoServer) setupServers() error {
	isMultiHost := hs.c.hugo().IsMultihost()

	var (
		baseURLs []string
		roots    []string
		ports    []int
	)

	currentServerPort := hs.c.Cfg.GetInt("previewPort")
	if isMultiHost {
		for _, s := range hs.c.hugo().Sites {
			baseURLs = append(baseURLs, s.BaseURL.String())
			roots = append(roots, s.Language().Lang)
			ports = append(ports, currentServerPort)
		}
	} else {
		s := hs.c.hugo().Sites[0]
		baseURLs = []string{s.BaseURL.String()}
		roots = []string{""}
		ports = []int{currentServerPort}
	}

	hs.c.Set("port", hs.c.Cfg.GetInt("previewPort"))
	hs.c.Set("liveReloadPort", ports[0])

	// Cache it here. The HugoSites object may be unavaialble later on due to intermitent configuration errors.
	// To allow the en user to change the error template while the server is running, we use
	// the freshest template we can provide.
	var (
		errTempl     tpl.Template
		templHandler tpl.TemplateHandler
	)
	getErrorTemplateAndHandler := func(h *hugolib.HugoSites) (tpl.Template, tpl.TemplateHandler) {
		if h == nil {
			return errTempl, templHandler
		}
		templHandler := h.Tmpl()
		errTempl, found := templHandler.Lookup("_server/error.html")
		if !found {
			panic("template server/error.html not found")
		}
		return errTempl, templHandler
	}
	errTempl, templHandler = getErrorTemplateAndHandler(hs.c.hugo())

	hs.baseURLs = baseURLs
	hs.roots = roots
	hs.ports = ports
	hs.errorTemplate = func(ctx any) (io.Reader, error) {
		// hugoTry does not block, getErrorTemplateAndHandler will fall back
		// to cached values if nil.
		templ, handler := getErrorTemplateAndHandler(hs.c.hugoTry())
		b := &bytes.Buffer{}
		err := handler.Execute(templ, b, ctx)
		return b, err
	}

	doLiveReload := !hs.c.Cfg.GetBool("disableLiveReload")

	if doLiveReload {
		livereload.Initialize()
	}

	for i := range baseURLs {
		mu, _ := hs.createEndpoint(i)

		u, err := url.Parse(helpers.SanitizeURL(baseURLs[i]))
		if err != nil {
			return err
		}

		if doLiveReload {
			mu.HandleFunc(u.Path+"/livereload.js", livereload.ServeJS)
			mu.HandleFunc(u.Path+"/livereload", livereload.Handler)
		}

		srv := &http.Server{
			Addr:    ":" + strconv.Itoa(hs.ports[i]),
			Handler: mu,
		}
		jww.FEEDBACK.Printf("Web Server is available at %s (bind address %s)\n", u.String(), hs.c.Cfg.GetString("bind"))

		hs.Servers = append(hs.Servers, srv)
	}
	return nil
}

func (hs *HugoServer) createEndpoint(i int) (*http.ServeMux, error) {
	baseURL := hs.baseURLs[i]
	root := hs.roots[i]
	port := hs.ports[i]

	// For logging only.
	// TODO(bep) consolidate.
	publishDir := hs.c.Cfg.GetString("publishDir")
	publishDirStatic := hs.c.Cfg.GetString("publishDirStatic")

	if root != "" {
		publishDir = filepath.Join(publishDir, root)
		publishDirStatic = filepath.Join(publishDirStatic, root)
	}
	// workingDir := hs.c.Cfg.GetString("workingDir")
	// absPublishDir := paths.AbsPathify(workingDir, publishDir)
	// absPublishDirStatic := paths.AbsPathify(workingDir, publishDirStatic)

	jww.FEEDBACK.Printf("Environment: %q", hs.c.hugo().Deps.Site.Hugo().Environment)

	if i == 0 {
		jww.FEEDBACK.Println("Serving pages from memory")
	}

	httpFs := afero.NewHttpFs(hs.c.publishDirServerFs)
	fs := filesOnlyFs{httpFs.Dir(path.Join("/", root))}

	if i == 0 && hs.c.fastRenderMode {
		jww.FEEDBACK.Println("Running in Fast Render Mode. For full rebuilds on change: hugo server --disableFastRender")
	}

	// We're only interested in the path
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("Invalid baseURL: %w", err)
	}

	decorate := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hs.c.showErrorInBrowser {
				// First check the error state
				err := hs.c.getErrorWithContext()
				if err != nil {
					hs.c.wasError = true
					w.WriteHeader(500)
					r, err := hs.errorTemplate(err)
					if err != nil {
						hs.c.logger.Errorln(err)
					}

					port = 1313
					if !hs.c.paused {
						port = hs.c.Cfg.GetInt("liveReloadPort")
					}
					lr := *u
					lr.Host = fmt.Sprintf("%s:%d", lr.Hostname(), port)
					fmt.Fprint(w, injectLiveReloadScript(r, lr))

					return
				}
			}

			// if hs.s.noHTTPCache {
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			// }

			// Ignore any query params for the operations below.
			requestURI := strings.TrimSuffix(r.RequestURI, "?"+r.URL.RawQuery)

			for _, header := range hs.c.serverConfig.MatchHeaders(requestURI) {
				w.Header().Set(header.Key, header.Value)
			}

			if redirect := hs.c.serverConfig.MatchRedirect(requestURI); !redirect.IsZero() {
				doRedirect := true
				// This matches Netlify's behaviour and is needed for SPA behaviour.
				// See https://docs.netlify.com/routing/redirects/rewrites-proxies/
				if !redirect.Force {
					path := filepath.Clean(strings.TrimPrefix(requestURI, u.Path))
					fi, err := hs.c.hugo().BaseFs.PublishFs.Stat(path)
					if err == nil {
						if fi.IsDir() {
							// There will be overlapping directories, so we
							// need to check for a file.
							_, err = hs.c.hugo().BaseFs.PublishFs.Stat(filepath.Join(path, "index.html"))
							doRedirect = err != nil
						} else {
							doRedirect = false
						}
					}
				}

				if doRedirect {
					if redirect.Status == 200 {
						if r2 := hs.rewriteRequest(r, strings.TrimPrefix(redirect.To, u.Path)); r2 != nil {
							requestURI = redirect.To
							r = r2
						}
					} else {
						w.Header().Set("Content-Type", "")
						http.Redirect(w, r, redirect.To, redirect.Status)
						return
					}
				}

			}

			if hs.c.fastRenderMode && hs.c.buildErr == nil {
				if strings.HasSuffix(requestURI, "/") || strings.HasSuffix(requestURI, "html") || strings.HasSuffix(requestURI, "htm") {
					if !hs.c.visitedURLs.Contains(requestURI) {
						// If not already on stack, re-render that single page.
						if err := hs.c.partialReRender(requestURI); err != nil {
							hs.c.handleBuildErr(err, fmt.Sprintf("Failed to render %q", requestURI))
							if hs.c.showErrorInBrowser {
								http.Redirect(w, r, requestURI, http.StatusMovedPermanently)
								return
							}
						}
					}

					hs.c.visitedURLs.Add(requestURI)

				}
			}

			h.ServeHTTP(w, r)
		})
	}

	fileserver := decorate(http.FileServer(fs))
	mu := http.NewServeMux()
	if u.Path == "" || u.Path == "/" {
		mu.Handle("/", fileserver)
	} else {
		mu.Handle(u.Path, http.StripPrefix(u.Path, fileserver))
	}

	return mu, nil
}

func (hs *HugoServer) rewriteRequest(r *http.Request, toPath string) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.URL = new(url.URL)
	*r2.URL = *r.URL
	r2.URL.Path = toPath
	r2.Header.Set("X-Rewrite-Original-URI", r.URL.RequestURI())

	return r2
}

/**
 * 获取页面模板信息
 * 参照 hugolib/page.go resolveTemplate
 * @return
 */
func ResolveTemplate(p page.Page, site *hugolib.Site, layouts ...string) ([]string, tpl.Template, bool, error) {
	var section string
	sections := p.SectionsEntries()

	switch p.Kind() {
	case page.KindSection:
		if len(sections) > 0 {
			section = sections[0]
		}
	case page.KindTaxonomy, page.KindTerm:
		// b := p.getTreeRef().n
		// section = b.viewInfo.name.singular
	default:
	}

	d := output.LayoutDescriptor{
		Kind:    p.Kind(),
		Type:    p.Type(),
		Lang:    p.Language().Lang,
		Layout:  p.Layout(),
		Section: section,
	}

	f := p.OutputFormats()[0].Format

	handler := output.NewLayoutHandler()
	findLayouts, _ := handler.For(d, f)

	if len(layouts) == 0 {
		selfLayout := p.Layout()
		if selfLayout != "" {
			selfLayout = selfLayout + f.Name
			if selfLayout != "" {
				templ, found := site.Tmpl().Lookup(selfLayout)
				return findLayouts, templ, found, nil
			}
		}
	}

	if len(layouts) > 0 {
		d.Layout = layouts[0]
		d.LayoutOverride = true
	}

	templ, found, err := site.Tmpl().LookupLayout(d, f)
	return findLayouts, templ, found, err
}
