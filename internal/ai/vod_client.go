package ai

import aiv1 "inno-live-server/api/gen/aiv1"

// VOD reuses the existing connection, message limits and worker lifecycle.
func (c *Client) VOD() aiv1.VodProcessorClient { return aiv1.NewVodProcessorClient(c.conn) }
