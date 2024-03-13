// Package v1 provides functionality for decoding Postgres requests and responses.
package v1

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func decodePostgres(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	pgRequests := [][]byte{reqBuf}
	errCh := make(chan error, 1)
	defer close(errCh)
	go func(errCh chan error, pgRequests [][]byte) {
		for {
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the pg client conn")
				errCh <- err
			}

			// To read the stream of request packets from the client
			for {
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
						if err == io.EOF {
							logger.Debug("EOF error received from client. Closing conn in postgres !!")
							errCh <- err
						}
						//TODO: why debug log sarthak?
						logger.Debug("failed to read the request message in proxy for postgres dependency")
						errCh <- err
					}
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					logger.Debug("the timeout for the client read in pg")
					break
				}
				pgRequests = append(pgRequests, buffer)
			}

			if len(pgRequests) == 0 {
				logger.Debug("the postgres request buffer is empty")
				continue
			}

			matched, pgResponses, err := matchingReadablePG(ctx, logger, pgRequests, mockDb)
			if err != nil {
				errCh <- fmt.Errorf("error while matching tcs mocks %v", err)
				return
			}

			if !matched {
				_, err = pUtil.PassThrough(ctx, logger, clientConn, dstCfg, pgRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to pass the request", zap.Any("request packets", len(pgRequests)))
					errCh <- err
				}
				continue
			}
			for _, pgResponse := range pgResponses {
				encoded, err := util.DecodeBase64(pgResponse.Payload)
				if len(pgResponse.PacketTypes) > 0 && len(pgResponse.Payload) == 0 {
					encoded, err = postgresDecoderFrontend(pgResponse)
				}
				if err != nil {
					utils.LogError(logger, err, "failed to decode the response message in proxy for postgres dependency")
					errCh <- err
				}
				_, err = clientConn.Write(encoded)
				if err != nil {
					utils.LogError(logger, err, "failed to write the response message to the client application")
					errCh <- err
				}
			}
			// Clear the buffer for the next dependency call
			pgRequests = [][]byte{}
		}
	}(errCh, pgRequests)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}