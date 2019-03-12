package logger

import (
	"encoding/json"
	"log"
)

// Println prints an object as json
func Println(obj interface{}) error {
	res, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	log.Printf("%s\n", res)
	return nil
}
