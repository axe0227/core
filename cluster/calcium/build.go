package calcium

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"time"

	enginetypes "github.com/projecteru2/core/engine/types"
	"github.com/projecteru2/core/log"
	"github.com/projecteru2/core/types"
	"github.com/projecteru2/core/utils"

	"github.com/pkg/errors"
)

// BuildImage will build image
func (c *Calcium) BuildImage(ctx context.Context, opts *types.BuildOptions) (ch chan *types.BuildImageMessage, err error) {
	logger := log.WithField("Calcium", "BuildImage").WithField("opts", opts)
	// Disable build API if scm not set
	if c.source == nil {
		return nil, logger.Err(ctx, errors.WithStack(types.ErrSCMNotSet))
	}
	// select nodes
	node, err := c.selectBuildNode(ctx)
	if err != nil {
		return nil, logger.Err(ctx, err)
	}
	log.Infof(ctx, "[BuildImage] Building image at pod %s node %s", node.Podname, node.Name)
	// get refs
	refs := node.Engine.BuildRefs(ctx, opts.Name, opts.Tags)

	switch opts.BuildMethod {
	case types.BuildFromSCM:
		ch, err = c.buildFromSCM(ctx, node, refs, opts)
	case types.BuildFromRaw:
		ch, err = c.buildFromContent(ctx, node, refs, opts.Tar)
	case types.BuildFromExist:
		ch, err = c.buildFromExist(ctx, refs[0], opts.ExistID, opts.User)
	default:
		return nil, logger.Err(ctx, errors.WithStack(errors.New("unknown build type")))
	}
	return ch, logger.Err(ctx, err)
}

func (c *Calcium) selectBuildNode(ctx context.Context) (*types.Node, error) {
	// get pod from config
	// TODO can choose multiple pod here for other engine support
	if c.config.Docker.BuildPod == "" {
		return nil, errors.WithStack(types.ErrNoBuildPod)
	}

	// get node by scheduler
	nodes, err := c.ListPodNodes(ctx, c.config.Docker.BuildPod, nil, false)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.WithStack(types.ErrInsufficientNodes)
	}
	// get idle max node
	node, err := c.scheduler.MaxIdleNode(nodes)
	return node, err
}

func (c *Calcium) buildFromSCM(ctx context.Context, node *types.Node, refs []string, opts *types.BuildOptions) (chan *types.BuildImageMessage, error) {
	buildContentOpts := &enginetypes.BuildContentOptions{
		User:   opts.User,
		UID:    opts.UID,
		Builds: opts.Builds,
	}
	path, content, err := node.Engine.BuildContent(ctx, c.source, buildContentOpts)
	defer os.RemoveAll(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	ch, err := c.buildFromContent(ctx, node, refs, content)
	return ch, err
}

func (c *Calcium) buildFromContent(ctx context.Context, node *types.Node, refs []string, content io.Reader) (chan *types.BuildImageMessage, error) {
	resp, err := node.Engine.ImageBuild(ctx, content, refs)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	ch, err := c.pushImage(ctx, resp, node, refs)
	return ch, err
}

func (c *Calcium) buildFromExist(ctx context.Context, ref, existID, user string) (chan *types.BuildImageMessage, error) { // nolint:unparam
	logger := log.WithField("Calcium", "buildFromExist").WithField("ref", ref).WithField("existID", existID)
	return withImageBuiltChannel(func(ch chan *types.BuildImageMessage) {
		node, err := c.getWorkloadNode(ctx, existID)
		if err != nil {
			ch <- buildErrMsg(logger.Err(ctx, err))
			return
		}

		imageID, err := node.Engine.ImageBuildFromExist(ctx, existID, ref, user)
		if err != nil {
			ch <- buildErrMsg(logger.Err(ctx, err))
			return
		}
		utils.SentryGo(func() {
			cleanupNodeImages(ctx, node, []string{imageID}, c.config.GlobalTimeout)
		})
		ch <- &types.BuildImageMessage{ID: imageID}
	}), nil
}

func (c *Calcium) pushImage(ctx context.Context, resp io.ReadCloser, node *types.Node, tags []string) (chan *types.BuildImageMessage, error) { // nolint:unparam
	logger := log.WithField("Calcium", "pushImage").WithField("node", node).WithField("tags", tags)
	return withImageBuiltChannel(func(ch chan *types.BuildImageMessage) {
		defer resp.Close()
		decoder := json.NewDecoder(resp)
		var lastMessage *types.BuildImageMessage
		for {
			message := &types.BuildImageMessage{}
			err := decoder.Decode(message)
			if err != nil {
				if err == io.EOF {
					break
				}
				if err == context.Canceled || err == context.DeadlineExceeded {
					log.Errorf(ctx, "[BuildImage] context timeout")
					lastMessage.ErrorDetail.Code = -1
					lastMessage.ErrorDetail.Message = err.Error()
					lastMessage.Error = err.Error()
					break
				}
				malformed, _ := ioutil.ReadAll(decoder.Buffered()) // TODO err check
				logger.Errorf(ctx, "[BuildImage] Decode build image message failed %+v, buffered: %v", err, malformed)
				return
			}
			ch <- message
			lastMessage = message
		}

		if lastMessage.Error != "" {
			log.Errorf(ctx, "[BuildImage] Build image failed %v", lastMessage.ErrorDetail.Message)
			return
		}

		// push and clean
		for i := range tags {
			tag := tags[i]
			log.Infof(ctx, "[BuildImage] Push image %s", tag)
			rc, err := node.Engine.ImagePush(ctx, tag)
			if err != nil {
				ch <- &types.BuildImageMessage{Error: logger.Err(ctx, err).Error()}
				continue
			}

			for message := range processBuildImageStream(ctx, rc) {
				ch <- message
			}

			// 无论如何都删掉build机器的
			// 事实上他不会跟cached pod一样
			// 一样就砍死
			ch <- &types.BuildImageMessage{Stream: fmt.Sprintf("finished %s\n", tag), Status: "finished", Progress: tag}
		}
		utils.SentryGo(func() {
			cleanupNodeImages(ctx, node, tags, c.config.GlobalTimeout)
		})
	}), nil

}

func withImageBuiltChannel(f func(chan *types.BuildImageMessage)) chan *types.BuildImageMessage {
	ch := make(chan *types.BuildImageMessage)
	utils.SentryGo(func() {
		defer close(ch)
		f(ch)
	})
	return ch
}

func cleanupNodeImages(ctx context.Context, node *types.Node, ids []string, ttl time.Duration) {
	logger := log.WithField("Calcium", "cleanupNodeImages").WithField("node", node).WithField("ids", ids).WithField("ttl", ttl)
	ctx, cancel := context.WithTimeout(utils.InheritTracingInfo(ctx, context.TODO()), ttl)
	defer cancel()
	for _, id := range ids {
		if _, err := node.Engine.ImageRemove(ctx, id, false, true); err != nil {
			logger.Errorf(ctx, "[BuildImage] Remove image error: %+v", errors.WithStack(err))
		}
	}
	if spaceReclaimed, err := node.Engine.ImageBuildCachePrune(ctx, true); err != nil {
		logger.Errorf(ctx, "[BuildImage] Remove build image cache error: %+v", errors.WithStack(err))
	} else {
		log.Infof(ctx, "[BuildImage] Clean cached image and release space %d", spaceReclaimed)
	}
}

func buildErrMsg(err error) *types.BuildImageMessage {
	msg := &types.BuildImageMessage{Error: err.Error()}
	msg.ErrorDetail.Message = err.Error()
	return msg
}
