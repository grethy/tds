// test package
package main

import (
	"bufio"
	"context"
	"database/sql/driver"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/thda/tds"

	"github.com/chzyer/readline"
	"github.com/thda/tablewriter"
)

var (
	version         = "0.01"
	noHeader        = false
	echoInput       = false
	noPSInInput     = false
	printVersion    = false
	chained         = false
	packetSize      = 512
	terminator      = ""
	database        = "master"
	hostname        string
	inputFile       string
	charset         string
	loginTimeout    = 60
	outputFile      string
	password        string
	columnSeparator = " "
	server          string
	commandTimeout  = 0
	pageSize        = 3000
	userName        string
	locale          string
	width           int
	ssl             = "off"
	theme           = "UtfCompact"
	re              *regexp.Regexp
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: example -stderrthreshold=[INFO|WARN|FATAL] -log_dir=[string]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func init() {
	hostname, _ = os.Hostname()
	flag.Usage = usage
	flag.BoolVar(&noHeader, "b", false, "disable headers")
	flag.BoolVar(&echoInput, "e", false, "print commands before execution")
	flag.BoolVar(&noPSInInput, "n", false, "no prompt is printed when displaying commands")
	flag.BoolVar(&printVersion, "v", false, "print version and exit")
	flag.BoolVar(&chained, "Y", false, "use chained mode. Can break lots of stored procedures")
	flag.IntVar(&packetSize, "A", 0, "custom network packet size. Zero to let the server handle it.")
	flag.StringVar(&terminator, "c", ";|^go", "the terminator used to determine the end of a command. Can contain regex.")
	flag.StringVar(&database, "D", database, "database to use.")
	flag.StringVar(&hostname, "H", "system hostname", "client's host name to send to the server.")
	flag.StringVar(&inputFile, "i", "/gsqlnone/", "file to read commands from")
	flag.StringVar(&charset, "J", charset, "character set")
	flag.StringVar(&theme, "T", theme, "display theme, can be ASCIICompact or UtfCompact")
	flag.IntVar(&loginTimeout, "l", 0, "login Timeout")
	flag.StringVar(&outputFile, "o", "/gsqlnone/", "file to output to")
	flag.StringVar(&password, "P", "none", "password")
	flag.IntVar(&pageSize, "p", pageSize, "paging size")
	flag.StringVar(&columnSeparator, "s", columnSeparator, "column separator")
	flag.StringVar(&server, "S", " ", "host:port")
	flag.IntVar(&commandTimeout, "t", 0, "command Timeout")
	flag.IntVar(&width, "w", 0, "line width")
	flag.StringVar(&userName, "U", "none", "user name")
	flag.StringVar(&ssl, "x", ssl, "Set to 'on' to enable ssl")
	flag.StringVar(&locale, "z", "none", "locale name")
	flag.Parse()

	re = regexp.MustCompile("(" + terminator + ")$")

	// check for mandatory parameters
	if userName == "" || server == "" {
		fmt.Fprintf(os.Stderr, "usage: example -stderrthreshold=[INFO|WARN|FATAL] -log_dir=[string]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}
}

// build the connection string
func buildCnxStr() string {
	// build the url
	v := url.Values{}
	if chained {
		v.Set("mode", "chained")
	}
	if packetSize != 0 {
		v.Set("packetSize", fmt.Sprintf("%d", packetSize))
	}
	if ssl == "on" {
		v.Set("ssl", "on")
	}
	v.Set("hostname", hostname)
	v.Set("readTimeout", "10")
	if charset != "" {
		v.Set("charset", charset)
	}
	return "tds://" + url.QueryEscape(userName) + ":" + url.QueryEscape(password) +
		"@" + server + "/" + url.QueryEscape(database) + "?" + v.Encode()
}

// find the string terminator in a line and add it to the current batch if needed
func processLine(terminator string, line string, batch string) (batchOut string, found bool) {
	// continue till we get a the terminator
	if match, _ := regexp.MatchString(terminator+"$", line); !match {
		if batch == "" {
			batchOut = line
		} else {
			// add the line to the batch
			batchOut = batch + "\n" + line
		}
		return batchOut, false
	}
	return batch + re.ReplaceAllString(line, ""), true
}

type SQLBatchReader interface {
	ReadBatch(terminator string) (batch string, err error)
	Close() error
}

type fileBatchReader struct {
	io.ReadCloser
	scanner *bufio.Reader
	w       *bufio.Writer
}

func (r *fileBatchReader) ReadBatch(terminator string) (batch string, err error) {
	found := false
	lineNo := 1
	batch = ""
	for {
		line, err := r.scanner.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return batch, err
		}
		batch, found = processLine(terminator, line, batch)

		// found the separator
		if found {
			lineNo = 1
			return batch, nil
		}

		if echoInput {
			fmt.Printf("%d> %s", lineNo, line)
		}
		lineNo++
	}
}

// get an instance of readline with the proper settings
func newFileBatchReader(inputFile string, w *bufio.Writer) (r *fileBatchReader, err error) {
	r = &fileBatchReader{w: w}
	if r.ReadCloser, err = os.Open(inputFile); err != nil {
		return nil, err
	}
	r.scanner = bufio.NewReader(r.ReadCloser)
	return r, nil
}

type readLineBatchReader struct {
	*readline.Instance
	server string
	conn   *tds.Conn
}

func (r *readLineBatchReader) ReadBatch(terminator string) (batch string, err error) {
	found := false
	lineNo := 1
	for {
		var prompt string
		switch r.conn.GetEnv()["serverType"] {
		case "ASE", "sql server":
			if r.server == "" {
				serverQuery, err := r.conn.SelectValue(context.Background(), "select @@servername")
				if err == nil {
					r.server = serverQuery.(string)
				}
			}
			prompt = fmt.Sprintf("%s.%s %d $ ", r.server, r.conn.GetEnv()["database"], lineNo)
		case "SQL Anywhere":
			if r.server == "" {
				serverQuery, err := r.conn.SelectValue(context.Background(), "select @@servername")
				if err == nil {
					r.server = serverQuery.(string)
				}
			}
			prompt = fmt.Sprintf("%s %d $ ", r.server, lineNo)
		default:
			r.server = r.conn.GetEnv()["server"]
			prompt = fmt.Sprintf("%s %d $ ", r.server, lineNo)
		}
		r.SetPrompt(prompt)
		line, err := r.Readline()

		if err == readline.ErrInterrupt {
			lineNo = 1
			batch = ""
			continue
		}
		if err != nil {
			return "", err
		}

		batch, found = processLine(terminator, line, batch)
		if found {
			lineNo = 1
			r.SaveHistory(batch)
			return batch, nil
		}
		lineNo++
	}
}

// get an instance of readline with the proper settings
func newReadLineBatchReader(conn *tds.Conn) (SQLBatchReader, error) {
	usr, _ := user.Current()
	rl, err := readline.NewEx(&readline.Config{
		Prompt:                 "$ ",
		HistoryFile:            usr.HomeDir + "/.gsql_history.txt",
		DisableAutoSaveHistory: true,
	})

	rl.SetPrompt("1> ")

	if err != nil {
		return nil, fmt.Errorf("newReadLine: error while initiating readline object (%s)", err)
	}

	return &readLineBatchReader{Instance: rl, conn: conn}, err
}

func newTable(out io.Writer) (table *tablewriter.Table) {
	table = tablewriter.New(out)
	switch theme {
	default:
	case "ASCIICompact":
		table.Theme = tablewriter.ASCIICompact
	case "UtfCompact":
		table.Theme = tablewriter.UtfCompact
	}
	//table.SetColWidth(10000)
	table.RowSep = false
	return table
}

func main() {
	// defer profile.Start(profile.CPUProfile).Stop()
	var batch string
	var r SQLBatchReader
	var w *bufio.Writer

	// connect
	conn, err := tds.NewConn(buildCnxStr())
	if err != nil {
		fmt.Println("failed to connect: ", err)
		os.Exit(1)
	}

	// print showplan messages and all
	conn.SetErrorhandler(func(m tds.SybError) bool {
		if m.Severity == 10 {
			if (m.MsgNumber >= 3612 && m.MsgNumber <= 3615) ||
				(m.MsgNumber >= 6201 && m.MsgNumber <= 6299) ||
				(m.MsgNumber >= 10201 && m.MsgNumber <= 10299) {
				fmt.Printf(m.Message)
			} else {
				fmt.Println(strings.TrimRight(m.Message, "\n"))
			}
		}

		if m.Severity > 10 {
			fmt.Print(m)
		}
		return m.Severity > 10
	})

	// open outpout
	switch outputFile {
	default:
		var f io.WriteCloser
		var _, err = os.Stat(outputFile)
		if os.IsNotExist(err) {
			// not found...
			f, err = os.Create(outputFile)
		} else {
			// truncate the file if it exists
			if err = os.Truncate(outputFile, 0); err == nil {
				f, err = os.Open(outputFile)
			}
		}
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		defer f.Close()
		w = bufio.NewWriter(f)

	case "/gsqlnone/":
		w = bufio.NewWriter(os.Stdout)

	}

	// open input
	switch inputFile {
	case "/gsqlnone/":
		// get readline instance
		r, err = newReadLineBatchReader(conn)
	default:
		r, err = newFileBatchReader(inputFile, w)
	}

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer r.Close()

input:
	for {
		batch, err = r.ReadBatch(terminator)
		switch batch {
		case "\\b":
			conn.Begin()
			continue input
		case "\\c":
			conn.Commit()
			continue input
		case "\\r":
			conn.Rollback()
			continue input
		}
		if err != nil {
			if err != io.EOF {
				fmt.Println(err)
			}
			break
		}

		// handle cancelation
		ctx, cancel := context.WithCancel(context.Background())

		c := make(chan os.Signal)
		done := make(chan struct{})
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			select {
			case <-c:
				cancel()
				<-done
			case <-done:
			}
		}()

		// send query
		rows, err := conn.QueryContext(ctx, batch, nil)
		select {
		case <-done:
		case done <- struct{}{}:
		}

		if err != nil {
			// SQL errors are printed by the error handler
			if _, ok := err.(tds.SybError); !ok {
				fmt.Println(err)
			}
			continue input
		}

		for {
			// init output table
			table := newTable(w)

			cols := rows.Columns()

			if cols == nil {
				continue input
			}
			table.SetHeader(cols)

			vals := make([]driver.Value, len(cols))
			data := make([]string, len(cols))
			r := 0
			for {
				err = rows.Next(vals)

				if err == io.EOF {
					break
				} else if err != nil {
					break
				}
				r++
				for i := 0; i < len(cols); i++ {
					if vals[i] == nil {
						vals[i] = "NULL"
					}
					// pretty print time/bytes
					if t, ok := vals[i].(time.Time); ok {
						vals[i] = t.Format("2006-01-02 15:04:05")
					}
					if b, ok := vals[i].([]byte); ok {
						vals[i] = "0x" + hex.EncodeToString(b)
					}
					data[i] = strings.TrimSpace(fmt.Sprint(vals[i]))
				}
				table.Append(data)
				if r%pageSize == 0 {
					table.Render()
					table = newTable(w)
					table.SetHeader(cols)
				}
			}

			if len(data) > 0 && len(cols) > 0 {
				table.Render()
			}

			// print return status
			affected, okAffected := rows.(*tds.Rows).AffectedRows()
			returnStatus, okReturnStatus := rows.(*tds.Rows).ReturnStatus()
			var display string

			if okAffected {
				if affected > 1 {
					display = fmt.Sprintf("%d rows affected", affected)
				} else {
					display = fmt.Sprintf("%d row affected", affected)
				}
			}

			if okReturnStatus {
				if okAffected {
					display += ", "
				}
				display += fmt.Sprintf("return status = %d", returnStatus)
			}

			if okReturnStatus || okAffected {
				fmt.Fprintln(w, "("+display+")")
			}

			w.Flush()

			// check for next result set
			if rows.(*tds.Rows).HasNextResultSet() {
				if err = rows.(*tds.Rows).NextResultSet(); err != nil {
					return
				}
				fmt.Println()
			} else {
				break
			}
		}
	}
}