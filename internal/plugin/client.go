package plugin

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	pluginpb "github.com/Tencent/WeKnora/internal/plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is the gRPC client used by the host to invoke a running plugin.
type Client struct {
	conn       *grpc.ClientConn
	lifecycle  pluginpb.PluginLifecycleClient
	datasource pluginpb.DataSourcePluginClient
}

func Dial(ctx context.Context, address string) (*Client, error) {
	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	}
	if strings.HasPrefix(address, "unix://") {
		socketPath := strings.TrimPrefix(address, "unix://")
		dialOptions = append(dialOptions, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}))
	}
	conn, err := grpc.DialContext(ctx, address, dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("dial plugin gRPC endpoint %q: %w", address, err)
	}
	return &Client{
		conn:       conn,
		lifecycle:  pluginpb.NewPluginLifecycleClient(conn),
		datasource: pluginpb.NewDataSourcePluginClient(conn),
	}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) HealthCheck(ctx context.Context) (*pluginpb.HealthCheckResponse, error) {
	return c.lifecycle.HealthCheck(ctx, &pluginpb.HealthCheckRequest{})
}

func (c *Client) GetInfo(ctx context.Context) (*pluginpb.PluginInfo, error) {
	return c.lifecycle.GetInfo(ctx, &pluginpb.GetInfoRequest{})
}

func (c *Client) ValidateConfig(ctx context.Context, config map[string]string) (*pluginpb.ValidateConfigResponse, error) {
	return c.lifecycle.ValidateConfig(ctx, &pluginpb.ValidateConfigRequest{Config: config})
}

func (c *Client) ValidateCredentials(ctx context.Context, config map[string]string) (*pluginpb.ValidateCredentialsResponse, error) {
	return c.datasource.ValidateCredentials(ctx, &pluginpb.ValidateCredentialsRequest{Config: config})
}

func (c *Client) Sync(ctx context.Context, datasourceID string, config map[string]string, cursor string) (pluginpb.DataSourcePlugin_SyncClient, error) {
	return c.datasource.Sync(ctx, &pluginpb.SyncRequest{
		DatasourceId: datasourceID,
		Config:       config,
		Cursor:       cursor,
	})
}

// CheckHealth verifies that a plugin endpoint is reachable and reports serving.
func CheckHealth(ctx context.Context, address string, timeout time.Duration) error {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := Dial(checkCtx, address)
	if err != nil {
		return err
	}
	defer client.Close()

	response, err := client.HealthCheck(checkCtx)
	if err != nil {
		return fmt.Errorf("call plugin health check: %w", err)
	}
	if response.Status != pluginpb.HealthCheckResponse_STATUS_SERVING {
		return fmt.Errorf("plugin is not serving: %s", response.Message)
	}
	return nil
}
