we are going to create a new go project in this directory.  it's an app to allow a mobile device to act as a multitouch trackpad for a linux desktop PC.  we will implement in several stages. 

stage 1: DONE

a go program acts as webserver.  it serves a web page to mobile clients.  there is a javascript program on the page.  it uses top half of the screen as the trackpad.  any touches, moves, end-touches, etc are detected and logged to console.  it communicates with the go server using webrtc data channel (for low latency UDP). each event is sent to the server and server prints the event at the terminal console.
