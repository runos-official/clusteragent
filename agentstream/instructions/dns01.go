package instructions

import (
	"fmt"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/commons"
	"log"
)

func Dns01ToServer(key string) *l2sec.FromClusterAgent {
	type Dns01ToServer struct {
		Key string `json:"key"`
	}

	log.Printf("Dns01ToServer key to server: %s", key)

	message, err := commons.JsonB64Encode(Dns01ToServer{Key: key})
	if err != nil {
		fmt.Println("Error encoding message: ", err)
	}

	return &l2sec.FromClusterAgent{
		Type:    "Dns01ToServer",
		JsonB64: message,
	}
}
