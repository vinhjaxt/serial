package serial

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

const rxDataTimeout = 20 * time.Millisecond

/*******************************************************************************************
*******************************   TYPE DEFINITIONS 	****************************************
*******************************************************************************************/

type SerialPort struct {
	fileLog    *log.Logger
	ReadingRx  sync.Mutex
	NeedRx     int32
	Port       io.ReadWriteCloser
	PortIsOpen int32
	Verbose    bool
	OnRxData   func([]byte)
	rxChar     chan byte
	rxData     chan string
	rxTimer    <-chan time.Time
}

/*******************************************************************************************
********************************   BASIC FUNCTIONS  ****************************************
*******************************************************************************************/

func New() *SerialPort {
	// Create new file
	return &SerialPort{
		Verbose:    true,
		OnRxData:   nil,
		PortIsOpen: 0,
		ReadingRx:  sync.Mutex{},
		fileLog:    nil,
	}
}

func (sp *SerialPort) Open(name string, baud int, timeout ...time.Duration) error {
	// Check if port is open
	if atomic.LoadInt32(&sp.PortIsOpen) == 1 {
		return fmt.Errorf("\"%s\" is already open", name)
	}
	var readTimeout time.Duration
	if len(timeout) > 0 {
		readTimeout = timeout[0]
	}
	// Open serial port
	comPort, err := openPort(name, baud, readTimeout)
	if err != nil {
		return fmt.Errorf("Unable to open port \"%s\" - %s", name, err)
	}
	atomic.StoreInt32(&sp.PortIsOpen, 1)
	if sp.Verbose == false {
		file, err := os.OpenFile(fmt.Sprintf("log_serial_%d.txt", time.Now().Unix()), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalln("Failed to open log file:", err)
		}
		sp.fileLog = log.New(file, "PREFIX: ", log.Ldate|log.Ltime)
		sp.fileLog.SetPrefix(fmt.Sprintf("[%s] ", name))
	}
	// Open port succesfull
	sp.Port = comPort
	// Open channels
	sp.rxChar = make(chan byte)
	sp.rxData = make(chan string)
	// Enable threads
	go sp.readSerialPort()
	go sp.processSerialPort()
	sp.log("Serial port %s@%d open", name, baud)
	return nil
}

// Close close the current Serial Port.
func (sp *SerialPort) Close() error {
	sp.Println("\x1A")
	if atomic.LoadInt32(&sp.PortIsOpen) == 1 {
		atomic.StoreInt32(&sp.PortIsOpen, 0)
		go func() {
			defer func() {
				recover()
			}()
			close(sp.rxChar)
		}()
		go func() {
			defer func() {
				recover()
			}()
			close(sp.rxData)
		}()
		sp.log("Serial port closed")
		return sp.Port.Close()
	}
	return nil
}

// This method prints data trough the serial port.
func (sp *SerialPort) Write(data []byte) (n int, err error) {
	if atomic.LoadInt32(&sp.PortIsOpen) == 1 {
		n, err = sp.Port.Write(data)
		if err != nil {
			// Do nothing
		} else {
			sp.log("Tx >> %s", string(data))
		}
	} else {
		err = fmt.Errorf("Serial port is not open")
	}
	return
}

// This method prints data trough the serial port.
func (sp *SerialPort) Print(str string) error {
	if atomic.LoadInt32(&sp.PortIsOpen) == 1 {
		_, err := sp.Port.Write([]byte(str))
		if err != nil {
			return err
		} else {
			sp.log("Tx >> %s", str)
		}
	} else {
		return fmt.Errorf("Serial port is not open")
	}
	return nil
}

// Prints data to the serial port as human-readable ASCII text followed by a carriage return character
// (ASCII 13, CR, '\r') and a newline character (ASCII 10, LF, '\n').
func (sp *SerialPort) Println(str string) error {
	return sp.Print(str + "\r\n")
}

// Printf formats according to a format specifier and print data trough the serial port.
func (sp *SerialPort) Printf(format string, args ...interface{}) error {
	str := format
	if len(args) > 0 {
		str = fmt.Sprintf(format, args...)
	}
	return sp.Print(str)
}

//This method send a binary file trough the serial port. If EnableLog is active then this method will log file related data.
func (sp *SerialPort) SendFile(filepath string) error {
	// Aux Vars
	sentBytes := 0
	q := 512
	data := []byte{}
	// Read file
	file, err := ioutil.ReadFile(filepath)
	if err != nil {
		sp.log("DBG >> %s", "Invalid filepath")
		return err
	} else {
		fileSize := len(file)
		sp.log("INF >> %s", "File size is %d bytes", fileSize)

		for sentBytes <= fileSize {
			//Try sending slices of less or equal than 512 bytes at time
			if len(file[sentBytes:]) > q {
				data = file[sentBytes:(sentBytes + q)]
			} else {
				data = file[sentBytes:]
			}
			// Write binaries
			_, err := sp.Port.Write(data)
			if err != nil {
				sp.log("DBG >> %s", "Error while sending the file")
				return err
			} else {
				sentBytes += q
				time.Sleep(time.Millisecond * 100)
			}
		}
	}
	//Encode data to send
	return nil
}

// Wait for a defined regular expression for a defined amount of time.
func (sp *SerialPort) WaitForRegexTimeout(cmd, exp string, timeout time.Duration) ([]string, error) {

	if atomic.LoadInt32(&sp.PortIsOpen) == 1 {
		//Decode received data
		timeExpired := false

		regExpPatttern := regexp.MustCompile(exp)

		//Timeout structure
		c1 := make(chan []string, 1)
		sp.ReadingRx.Lock()
		atomic.StoreInt32(&sp.NeedRx, 1)
		go func() {
			sp.log("INF >> Waiting for RegExp: \"%s\"", exp)
			result := []string{}
			lines := ""
			for !timeExpired {
				lines += <-sp.rxData
				result = regExpPatttern.FindStringSubmatch(lines)
				if len(result) > 0 {
					lines = ""
					c1 <- result
					break
				}
			}
			atomic.StoreInt32(&sp.NeedRx, 0)
			sp.ReadingRx.Unlock()
		}()

		// Send command
		if cmd != "" {
			if err := sp.Println(cmd); err != nil {
				return nil, err
			}
		}
		select {
		case data := <-c1:
			sp.log("INF >> The RegExp: \"%s\"", exp)
			sp.log("INF >> Has been matched: %q", data[0])
			return data, nil
		case <-time.After(timeout):
			timeExpired = true
			sp.log("INF >> Unable to match RegExp: \"%s\"", exp)
			return nil, fmt.Errorf("Timeout expired \"%s\"", exp)
		}
	} else {
		return nil, fmt.Errorf("Serial port is not open")
	}
}

/*******************************************************************************************
******************************   PRIVATE FUNCTIONS  ****************************************
*******************************************************************************************/

func (sp *SerialPort) readSerialPort() {
	rxBuff := make([]byte, 256)
	for atomic.LoadInt32(&sp.PortIsOpen) == 1 {
		n, _ := sp.Port.Read(rxBuff)
		for _, b := range rxBuff[:n] {
			if atomic.LoadInt32(&sp.PortIsOpen) == 1 {
				sp.rxChar <- b
			}
		}
	}
}

func (sp *SerialPort) processSerialPort() {
	var screenBuff []byte
	var lastRxByte byte

	for {
		if atomic.LoadInt32(&sp.PortIsOpen) == 1 {
			select {
			case lastRxByte = <-sp.rxChar:
				screenBuff = append(screenBuff, lastRxByte)
				sp.rxTimer = time.After(rxDataTimeout)
				break
			case <-sp.rxTimer:
				if screenBuff == nil {
					break
				}
				if sp.OnRxData != nil {
					sp.OnRxData(screenBuff)
				}

				sp.log("Rx << %q", screenBuff)

				if atomic.LoadInt32(&sp.NeedRx) == 1 {
					sp.rxData <- string(screenBuff)
				}
				screenBuff = nil //Clean buffer
				break
			}
		} else {
			break
		}
	}
}

func (sp *SerialPort) log(format string, a ...interface{}) {
	if sp.Verbose {
		log.Printf(format, a...)
	} else if sp.fileLog != nil {
		sp.fileLog.Printf(format, a...)
	}
}
