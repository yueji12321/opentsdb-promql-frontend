package main

import (
        "net/http"
        "os"
        "strings"

        v1API "github.com/yueji12321/opentsdb-promql-frontend/api/v1"
        "github.com/prometheus/common/log"
        "github.com/prometheus/common/route"
        "github.com/prometheus/prometheus/promql"
        "golang.org/x/net/context"
)

const apiRoute = "/api/v1"

var config = struct {
        listenAddr  string
        openTSDBurl string
}{
        "localhost:9080",
        "http://localhost:4242",
}

func init() {
        if len(os.Getenv("ADDR")) > 0 {
                config.listenAddr = os.Getenv("ADDR")
        }
        if len(os.Getenv("OPENTSDB_URL")) > 0 {
                config.openTSDBurl = os.Getenv("OPENTSDB_URL")
        }
}

func main() {
        arg_len := len(os.Args)
        for i := 1 ; i < arg_len ; i++{
                str := strings.Split(os.Args[i],"=")
                if(len(str) !=2){
                        log.Infof("Error, flag is unkown!\n")
                        os.Exit(2)
                }
                flag := str[0]
                value := str[1]
                switch{
                        case flag == "ADDR":
                                config.listenAddr = value
                        case flag == "OPENTSDB_URL":
                                config.openTSDBurl = value
                        default:
                                log.Infof("Error, flag is unkown!\n")
                                os.Exit(2)
                }
        }
        var (
                ctx, cancelCtx = context.WithCancel(context.Background())
                storage        = &queryable{}
                queryEngine    = promql.NewEngine(storage, promql.DefaultEngineOptions)
        )
        defer cancelCtx()

        router := route.New(func(r *http.Request) (context.Context, error) {
                return ctx, nil
        })

        var api = v1API.NewAPI(queryEngine, storage, nil, nil)
        api.Register(router.WithPrefix(apiRoute))

        log.Infof("Listening on %s, will connect to OpenTSDB at %s", config.listenAddr, config.openTSDBurl)
        log.Fatal(http.ListenAndServe(config.listenAddr, router))
}
