import * as React from 'react'
import { Dispatch } from 'redux'
import { connect } from 'react-redux'

import { ApplicationState } from 'types/schema'
import { events } from 'state/events'
import { Starlightd } from 'lib/starlightd'
import {
  ClientState,
  ClientResponse,
  UpdateHandler,
  ResponseHandler,
} from 'starlight-sdk'
import { checkUnauthorized } from 'state/lifecycle'

const client = Starlightd.client

interface Props {
  updateHandler: UpdateHandler
  responseHandler: ResponseHandler
  clientState: ClientState
  dispatchLogout: () => void
}

class EventLoop extends React.Component<Props, {}> {
  public async componentWillMount() {
    client.clientState = this.props.clientState
    client.subscribe(this.props.updateHandler)
    client.responseHandler = this.props.responseHandler
  }

  public render() {
    return this.props.children
  }

  public async componentWillUnmount() {
    client.responseHandler = undefined
    client.unsubscribe()
  }
}

const mapStateToProps = (state: ApplicationState) => {
  return {
    clientState: state.events.clientState,
  }
}

const mapDispatchToProps = (dispatch: Dispatch) => {
  return {
    updateHandler: events.getHandler(dispatch),
    responseHandler: (response: ClientResponse) => {
      return checkUnauthorized(response, dispatch)
    },
  }
}

export const ConnectedEventLoop = connect<{}, {}, {}>(
  mapStateToProps,
  mapDispatchToProps
)(EventLoop)
