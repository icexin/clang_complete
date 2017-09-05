package main

import (
	"bufio"
	"bytes"
	"container/list"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	searchroots   stringSlice
	ccflags       stringSlice
	srcExtFlag    = flag.String("src_suffix", ".c .cc .cpp", "suffix of src or header file")
	headerExtFlag = flag.String("header_suffix", ".h .hpp", "suffix of include file")
	output        = flag.String("o", ".clang_complete", "output file, '-' means stdout")
	printSystem   = flag.Bool("sys", true, "print system headers get from 'gcc -xc++ -E -v -'")
	nworks        = flag.Int("work", runtime.NumCPU(), "works default number of cpus")
	debugon       = flag.Bool("v", false, "turn on debug")
)

var (
	errSkip     = errors.New("skip")
	errNotFound = errors.New("not found")
	log         = &logger{}
)

type stringSlice []string

func (s *stringSlice) String() string {
	return fmt.Sprintf("%q", []string(*s))
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type logger struct {
	id int
}

func (l *logger) New() *logger {
	l.id++
	return &logger{id: l.id}
}

func (l *logger) Debug(fmtstr string, args ...interface{}) {
	if *debugon {
		buf := new(bytes.Buffer)
		fmt.Fprintf(buf, "[%08d] [%s]", l.id, time.Now().Format("15:04:05"))
		fmt.Fprintf(buf, fmtstr, args...)
		fmt.Fprint(buf, "\n")
		os.Stderr.Write(buf.Bytes())
	}
}

func (l *logger) Fatal(args ...interface{}) {
	fmt.Fprint(os.Stderr, args...)
	os.Exit(-1)
}

type node struct {
	lock       sync.Mutex
	Name       string
	ParentPath string
	Children   map[string][]*node
}

func newNode(name string, parentPath string) *node {
	return &node{
		Name:       name,
		ParentPath: parentPath,
		Children:   make(map[string][]*node),
	}
}

func (n *node) AddChild(child *node) {
	n.lock.Lock()
	defer n.lock.Unlock()

	l := n.Children[child.Name]
	l = append(l, child)
	n.Children[child.Name] = l
}

func (n *node) Path() string {
	return filepath.Join(n.ParentPath, n.Name)
}

type tree struct {
	roots map[string]*node
}

func newTree() *tree {
	return &tree{
		roots: make(map[string]*node),
	}
}

func (t *tree) Scan(p string, acceptext map[string]bool) error {
	p, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	root := newNode("", "")
	_, err = t.buildtree(p, root, acceptext)
	if err != nil && err != errSkip {
		return err
	}
	t.roots[p] = root
	return nil
}

func (t *tree) Search(header string) ([]string, error) {
	if len(header) > 0 && header[0] == '/' {
		header = header[1:]
	}
	seps := strings.Split(header, string(filepath.Separator))

	var nodelist []*node
	for _, root := range t.roots {
		nodelist = append(nodelist, root)
	}

	for i := len(seps) - 1; i >= 0; i-- {
		name := seps[i]
		var nodelist1 []*node
		for _, n := range nodelist {
			l, ok := n.Children[name]
			if !ok {
				continue
			}
			nodelist1 = append(nodelist1, l...)
		}
		if len(nodelist1) == 0 {
			return nil, errNotFound
		}
		nodelist = nodelist1
	}

	var ret []string

	for _, n := range nodelist {
		ret = append(ret, filepath.Dir(n.Path()))
	}
	return ret, nil
}

func (t *tree) buildtree(p string, root *node, acceptext map[string]bool) (*node, error) {
	log := log.New()
	ppath, name := filepath.Split(p)
	if name[0] == '.' {
		return nil, errSkip
	}

	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}

	// skip strange files
	mode := info.Mode()
	if !mode.IsRegular() && !mode.IsDir() {
		return nil, errSkip
	}

	// 如果是文件则加入到根节点
	if mode.IsRegular() {
		ext := filepath.Ext(p)
		if !acceptext[ext] {
			return nil, errSkip
		}
		n := newNode(name, ppath)
		root.AddChild(n)
		return n, nil
	}

	log.Debug("scan dir %s", p)
	// 如果是目录，递归创建父节点，然后把自己加入父节点的子节点中
	files, err := ioutil.ReadDir(p)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errSkip
	}

	n := newNode(name, ppath)

	for _, file := range files {
		fullpath := filepath.Join(p, file.Name())
		parent, err := t.buildtree(fullpath, root, acceptext)
		if err != nil && err != errSkip {
			return nil, err
		}
		if err == errSkip {
			continue
		}
		parent.AddChild(n)
	}
	return n, nil
}

func isLocationKnownHeader(name string) bool {
	return filepath.IsAbs(name)
}

func listheaders(file string, acceptsuffix map[string]bool, includes []string) ([]string, error) {
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "gcc"
	}
	stderr := new(bytes.Buffer)

	flags := []string{"-xc++", "-M", "-MG"}
	flags = append(flags, ccflags...)
	flags = append(flags, includes...)
	flags = append(flags, file)
	cmd := exec.Command(cc, flags...)
	cmd.Stderr = stderr

	out, err := cmd.Output()
	if len(out) == 0 {
		return nil, fmt.Errorf("%s:%s", err, stderr.Bytes())
	}

	out = out[:len(out)-1]
	out = bytes.Replace(out, []byte("\\\n"), []byte{}, -1)
	list := bytes.Split(out, []byte(" "))

	var ret []string
	for _, header := range list[1:] {
		if len(header) == 0 {
			continue
		}
		s := string(header)
		if !acceptsuffix[filepath.Ext(s)] {
			continue
		}
		if isLocationKnownHeader(s) {
			continue
		}
		ret = append(ret, s)
	}

	return ret, nil
}

func collect(src string, l *list.List, acceptsuffix map[string]bool) error {
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := info.Name()
		if len(name) > 1 && name[0] == '.' {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(name)
		if !acceptsuffix[ext] {
			return nil
		}
		l.PushBack(path)
		return nil
	})
	return err
}

func systemheaders() ([]string, error) {
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "gcc"
	}
	cmd := exec.Command(cc, "-xc++", "-E", "-v", "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	var ret []string
	var started bool
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#include <...> search starts here:") {
			started = true
			continue
		}
		if strings.HasPrefix(line, "End of search list.") {
			break
		}

		if started {
			ret = append(ret, line)
		}

	}
	return ret, nil
}

func searchSystemHeader(name string, list []string) (string, error) {
	for _, dir := range list {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return dir, nil
		}
	}
	return "", errNotFound
}

type printer struct {
	w    io.WriteCloser
	lock sync.Mutex
	m    map[string]bool
	sys  []string
	l    []string
}

func newPrinter(w io.WriteCloser) *printer {
	return &printer{
		w: w,
		m: make(map[string]bool),
	}
}

func (p *printer) AddSys(sys []string) {
	p.sys = sys
}

func (p *printer) Printdirs(dirs []string) {
	p.lock.Lock()
	defer p.lock.Unlock()

	log := log.New()
	for _, h := range dirs {
		if !p.m[h] {
			log.Debug("new include dir: %s", h)
			p.m[h] = true
			p.l = append(p.l, h)
		}
	}
}

func (p *printer) Includes() []string {
	p.lock.Lock()
	defer p.lock.Unlock()

	var ret []string
	for dir := range p.m {
		ret = append(ret, "-I"+dir)
	}
	for _, dir := range p.sys {
		ret = append(ret, "-I"+dir)
	}
	return ret
}

func (p *printer) Flush() {
	p.lock.Lock()
	defer p.lock.Unlock()

	sort.Sort(sort.StringSlice(p.l))
	for _, h := range p.l {
		fmt.Fprintln(p.w, "-I"+h)
	}
}

func searchFile(p string, headerext map[string]bool, t *tree, printer *printer, lock *sync.Mutex, queue *list.List) {
	log := log.New()

	headers, err := listheaders(p, headerext, printer.Includes())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	log.Debug("process %s:%q", p, headers)

	if len(headers) == 0 {
		return
	}

	var reserve bool
	for _, h := range headers {
		// 首先尝试从搜索树中搜索
		dirs, err := t.Search(h)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s:%s\n", h, err)
			continue
		}
		reserve = true
		printer.Printdirs(dirs)
	}
	if reserve {
		lock.Lock()
		queue.PushBack(p)
		lock.Unlock()
	}
}

func main() {
	flag.Var(&searchroots, "s", "search root")
	flag.Var(&ccflags, "x", "extra cc flags")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("usage clang_complete [options] src_dir")
	}
	var err error
	srcroot := flag.Arg(0)
	srcroot, err = filepath.Abs(srcroot)
	if err != nil {
		log.Fatal(err)
	}

	var outf io.WriteCloser
	if *output == "-" {
		outf = os.Stdout
	} else {
		outf, err = os.Create(*output)
		if err != nil {
			log.Fatal(err)
		}
	}

	headerext := make(map[string]bool)
	for _, s := range strings.Split(*headerExtFlag, " ") {
		headerext[s] = true
	}
	srcext := make(map[string]bool)
	for _, s := range strings.Split(*srcExtFlag, " ") {
		srcext[s] = true
	}

	printer := newPrinter(outf)

	// 获取系统搜索目录
	sysheaders, err := systemheaders()
	if err != nil {
		log.Fatal(err)
	}
	printer.AddSys(sysheaders)

	if *printSystem {
		printer.Printdirs(sysheaders)
	}

	// 构造搜索树
	t := newTree()
	b := time.Now()
	for _, root := range searchroots {
		err = t.Scan(root, headerext)
		if err != nil {
			log.Fatal(err)
		}
	}
	tindex := time.Now().Sub(b)

	// 构造源码列表
	l := list.New()
	err = collect(srcroot, l, srcext)
	if err != nil {
		log.Fatal(err)
	}

	pool := newPool(*nworks)
	lock := new(sync.Mutex)
	// 广度优先搜索
	b = time.Now()
	for l.Len() != 0 {
		queue := list.New()
		for n := *nworks; l.Len() != 0 && n > 0; n-- {
			e := l.Front()
			l.Remove(e)
			p := e.Value.(string)
			rel, _ := filepath.Rel(srcroot, p)
			fmt.Fprintln(os.Stderr, rel)
			pool.Run(func() {
				searchFile(p, headerext, t, printer, lock, queue)
			})
		}
		pool.Wait()
		l.PushFrontList(queue)
	}
	tsearch := time.Now().Sub(b)
	printer.Flush()
	fmt.Fprintf(os.Stderr, "total:%.2fs index:%.2fs search:%.2fs\n",
		(tindex + tsearch).Seconds(), tindex.Seconds(), tsearch.Seconds())
}
