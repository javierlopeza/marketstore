// Package cli
// This file is the hub of the `cli` package. The `Client` struct defined here
// manages the database connection has the responsibility of interpreting user
// inputs.
package cli

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/alpacahq/marketstore/SQLParser"
	"github.com/alpacahq/marketstore/executor"
	"github.com/alpacahq/marketstore/frontend/client"
	"github.com/alpacahq/marketstore/utils"
	dbio "github.com/alpacahq/marketstore/utils/io"
	"github.com/chzyer/readline"
)

// mode is the client connection mode.
type mode int

const (
	local mode = iota
	remote
)

// target is the client's write target
type target int

const (
	terminal target = iota
	file
)

// Client represents an agent that manages a database
// connection and performs transactions specified by a
// user in a command-line buffer.
type Client struct {
	// timing flag determines to print query execution time.
	timing bool
	// target determines the target to write responses to.
	target target
	// mode determines local or remote.
	mode mode
	// url is the optional address of a db instance on a different machine.
	url string
	// rc is the optional remote client.
	rc *client.Client
	// dir is the optional filesystem location of a local db instance.
	dir string
	// outputDir is the optional location of the csv file to write to.
	outputDir string
}

// NewLocalClient builds a new client struct in local mode.
func NewLocalClient(dir string) (c *Client, err error) {
	// Configure db settings.
	initCatalog, initWALCache, backgroundSync, WALBypass := true, true, false, true
	utils.InstanceConfig.WALRotateInterval = 5
	executor.NewInstanceSetup(dir, initCatalog, initWALCache, backgroundSync, WALBypass)
	return &Client{dir: dir, mode: local}, nil
}

// NewRemoteClient generates a new client struct.
func NewRemoteClient(url string) (c *Client, err error) {
	// TODO: validate url using go core packages.
	splits := strings.Split(url, ":")
	if len(splits) != 2 {
		msg := fmt.Sprintf("incorrect URL, need \"hostname:port\", have: %s\n", url)
		return nil, errors.New(msg)
	}
	// build url.
	url = "http://" + url + "/"
	return &Client{url: url, mode: remote}, nil
}

// Connect initializes a client connection.
func (c *Client) Connect() error {
	// Nothing to do here yet..
	if c.mode == local {
		return nil
	}

	// Attempt connection to remote host.
	client, err := client.NewClient(c.url)
	if err != nil {
		return err
	}
	c.rc = client

	// Success.
	return nil
}

// Read kicks off the buffer reading process.
func (c *Client) Read() error {

	// Build reader.
	r, err := newReader()
	if err != nil {
		return err
	}
	defer r.Close()

	// Print connection information.
	if c.mode == local {
		fmt.Fprintf(os.Stderr, "Connected to local instance at path: %v\n", c.dir)
	} else {
		fmt.Fprintf(os.Stderr, "Connected to remote instance at: %v\n", c.url)
	}
	fmt.Fprintf(os.Stderr, "Type `\\help` to see command options\n")

	// User input evaluation loop.
EVAL:
	for {
		// Read input.
		line, err := r.Readline()

		// Terminate evaluation.
		if err == io.EOF {
			break EVAL
		}

		// Printed interrupt prompt.
		if err == readline.ErrInterrupt {
			continue
		}

		// Print error.
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			continue
		}

		// Remove leading/trailing spaces.
		line = strings.Trim(line, " ")

		// Evaulate.
		switch {
		// Flip timing flag.
		case strings.HasPrefix(line, "\\timing"):
			c.timing = !c.timing
		// Show.
		case strings.HasPrefix(line, "\\show"):
			c.show(line)
		// Trim.
		case strings.HasPrefix(line, "\\trim"):
			c.trim(line)
		// Gaps.
		case strings.HasPrefix(line, "\\gaps"):
			c.findGaps(line)
		// Load.
		case strings.HasPrefix(line, "\\load"):
			c.load(line)
		// Create.
		case strings.HasPrefix(line, "\\create"):
			c.create(line)
		// Function help.
		case strings.HasPrefix(line, "\\help") || strings.HasPrefix(line, "\\?"):
			c.functionHelp(line)
		// Reader help.
		case line == "help":
			c.functionHelp("\\help")
		// Quit.
		case line == "\\stop", line == "\\quit", line == "\\q", line == "exit":
			fmt.Println("got quit")
			break EVAL
			// Nothing to do.
		case line == "":
			continue EVAL
		// It was a sql stmt.
		default:
			c.sql(line)
		}
	}

	return nil
}

func newReader() (*readline.Instance, error) {
	// Determine history file path.
	usr, err := user.Current()
	if err != nil {
		return nil, errors.New("unable to obtain home directory")
	}
	history := filepath.Join(usr.HomeDir, ".marketstoreReaderHistory")

	// Register commands with autocompletion.
	autoComplete := readline.NewPrefixCompleter(
		readline.PcItem("\\show"),
		readline.PcItem("\\load"),
		readline.PcItem("\\create"),
		readline.PcItem("\\trim"),
		readline.PcItem("\\help"),
		readline.PcItem("\\exit"),
		readline.PcItem("\\quit"),
		readline.PcItem("\\q"),
		readline.PcItem("\\?"),
		readline.PcItem("\\stop"),
	)

	// Build config.
	config := &readline.Config{
		Prompt:          "\033[31m»\033[0m ",
		HistoryFile:     history,
		AutoComplete:    autoComplete,
		InterruptPrompt: "\nInterrupt, Press Ctrl+D to exit",
		EOFPrompt:       "exit",
	}

	// return reader.
	return readline.NewEx(config)
}

func printHeaderLine(cs *dbio.ColumnSeries) {
	fmt.Printf(formatHeader(cs, "="))
	fmt.Printf("\n")
}

func printColumnNames(colNames []string) {
	for i, name := range colNames {
		switch i {
		case 0:
			fmt.Printf("%29s  ", name)
		default:
			fmt.Printf("%-10s  ", name)
		}
	}
	fmt.Printf("\n")
}

func printResult(queryText string, cs *dbio.ColumnSeries, optional_writer ...*csv.Writer) (err error) {
	var writer *csv.Writer
	if len(optional_writer) != 0 {
		writer = optional_writer[0]
	}

	if cs == nil {
		fmt.Println("No results returned from query")
		return
	}
	/*
		Check if this is an EXPLAIN output
	*/
	i_explain := cs.GetByName("explain-output")
	if i_explain != nil {
		explain := i_explain.([]string)
		SQLParser.PrintExplain(queryText, explain)
		return
	}
	i_epoch := cs.GetByName("Epoch")
	if i_epoch == nil {
		return fmt.Errorf("Epoch column not present in output")
	}
	var epoch []int64
	var ok bool
	if epoch, ok = i_epoch.([]int64); !ok {
		return fmt.Errorf("Unable to convert Epoch column")
	}

	printHeaderLine(cs)
	printColumnNames(cs.GetColumnNames())
	printHeaderLine(cs)
	for i, ts := range epoch {
		row := []string{}
		var element string
		for _, name := range cs.GetColumnNames() {
			if strings.EqualFold(name, "Epoch") {
				fmt.Printf("%29s  ", dbio.ToSystemTimezone(time.Unix(ts, 0)).String()) // Epoch
				continue
			}
			col := cs.GetByName(name)
			colType := reflect.TypeOf(col).Elem().Kind()
			switch colType {
			case reflect.Float32:
				val := col.([]float32)[i]
				element = strconv.FormatFloat(float64(val), 'f', -1, 32)
			case reflect.Float64:
				val := col.([]float64)[i]
				element = strconv.FormatFloat(val, 'f', -1, 32)
			case reflect.Int32:
				val := col.([]int32)[i]
				element = strconv.FormatInt(int64(val), 10)
			case reflect.Int64:
				val := col.([]int64)[i]
				element = strconv.FormatInt(val, 10)
			case reflect.Uint8:
				val := col.([]byte)[i]
				element = strconv.FormatInt(int64(val), 10)
			}
			if writer != nil {
				row = append(row, element)
			} else {
				fmt.Printf("%-10s  ", element)
			}
		}
		fmt.Printf("\n")
		// write to csv
		if writer != nil {
			writer.Write(row)
			row = []string{}
		}
	}
	printHeaderLine(cs)
	return err
}

func formatHeader(cs *dbio.ColumnSeries, printChar string) string {
	var buffer bytes.Buffer
	appendChars := func(count int) {
		for i := 0; i < count; i++ {
			buffer.WriteString(printChar)
		}
		buffer.WriteString("  ")
	}
	for _, name := range cs.GetColumnNames() {
		if strings.EqualFold(name, "Epoch") {
			appendChars(29)
			continue
		}
		col := cs.GetByName(name)
		colType := reflect.TypeOf(col).Elem().Kind()
		switch colType {
		case reflect.Float32:
			appendChars(10)
		case reflect.Float64:
			appendChars(10)
		case reflect.Int32:
			appendChars(10)
		case reflect.Int64:
			appendChars(10)
		case reflect.Uint8:
			appendChars(10)
		}
	}
	return buffer.String()
}
